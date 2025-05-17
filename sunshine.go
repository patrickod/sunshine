package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/hhsnopek/etag"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	_ "modernc.org/sqlite"
	"tailscale.com/tsnet"
	"tailscale.com/tsweb"
)

var (
	port       = flag.Int("port", 8080, "http port to listen on")
	runAsTSNet = flag.Bool("tsnet", false, "run as a Tailscale net server")
	tsnetDir   = flag.String("tsnet-dir", "", "directory to store Tailscale state")
	//go:embed static/*
	staticFS embed.FS

	// Load templates
	departments *map[string]Department
	funcMap     = template.FuncMap{
		"url_for": func(name, filename string) string {
			switch name {
			case "static":
				return fmt.Sprintf("/static/%s", filename)
			default:
				return fmt.Sprintf("/%s", filename)
			}
		},
		"slugify": slugify,
	}

	//go:embed templates/*
	templateFS    embed.FS
	indexTemplate = template.Must(
		template.New("root").
			Funcs(funcMap).
			ParseFS(templateFS, "templates/base.html", "templates/index.html"))
	listTemplate = template.Must(
		template.New("root").
			Funcs(funcMap).
			ParseFS(templateFS, "templates/base.html", "templates/list.html"))
	departmentTemplate = template.Must(
		template.New("root").
			Funcs(funcMap).
			ParseFS(templateFS, "templates/base.html", "templates/department.html"))
	emailTemplateTemplate = template.Must(
		template.New("root").
			Funcs(funcMap).
			ParseFS(templateFS, "templates/base.html", "templates/email-template.html"))
)

// Define Prometheus metrics
var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Count of all HTTP requests",
		},
		[]string{"method", "path"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "Duration of all HTTP requests",
		},
		[]string{"method", "path"},
	)
	httpInFlightRequests = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "http_in_flight_requests",
		Help: "Current number of HTTP requests in flight",
	})
	httpRequestSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_request_size_bytes",
			Help: "Size of HTTP requests",
		},
		[]string{"method", "path"},
	)
	httpResponseSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_response_size_bytes",
			Help: "Size of HTTP responses",
		},
		[]string{"method", "path"},
	)
)

type Department struct {
	Name string `json:"name"`
	// Generated from name - not read from JSON
	NameSlug    string `json:"-"`
	Email       string `json:"email"`
	ContactName string `json:"contact_name"`
	Notes       string `json:"notes"`
	URL         string `json:"url"`
}

// LoggingMiddleware wraps an http.Handler and logs request summaries
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Call the next handler
		next.ServeHTTP(w, r)

		// Log the request summary
		duration := time.Since(start)
		log.Printf("%s - - [%s] \"%s %s %s\" %.3f\n",
			r.RemoteAddr,
			start.Format("02/Jan/2006:15:04:05 -0700"),
			r.Method,
			r.URL.Path,
			r.Proto,
			duration.Seconds(),
		)
	})
}

func loadDepartments() map[string]Department {
	var d map[string]Department
	f, err := staticFS.Open("static/departments.json")
	if err != nil {
		log.Fatalf("unable to read departments.json: %v", err)
	}
	if err := json.NewDecoder(f).Decode(&d); err != nil {
		log.Fatalf("unable to decode departments.json: %v", err)
	}
	return d
}

func createDB() *sql.DB {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		log.Fatalf("unable to open database: %v", err)
	}
	_, err = db.Exec(`CREATE VIRTUAL TABLE departments USING FTS5(
		name,
		name_slug,
		email,
		contact_name,
		notes,
		url
	)`)
	if err != nil {
		log.Fatalf("unable to create departments table: %v", err)
	}

	for name, dept := range *departments {
		_, err := db.Exec(`INSERT INTO departments (
			name,
			name_slug,
			email,
			url
		) VALUES (?, ?, ?, ?)`,
			name,
			slugify(name),
			dept.Email,
			dept.URL,
		)
		if err != nil {
			log.Fatalf("unable to insert department: %v", err)
		}
	}

	return db

}

type foiaServer struct {
	db *sql.DB
}

func (s *foiaServer) CreateMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.indexHandler)
	mux.HandleFunc("/list", s.listHandler)
	mux.HandleFunc("/email-template", s.emailTemplateHandler)
	mux.HandleFunc("/department/{id}", s.departmentHandler)
	mux.HandleFunc("/search", s.searchHandler)
	mux.Handle("/static/", http.FileServer(http.FS(staticFS)))
	return mux
}

func init() {
	prometheus.MustRegister(httpRequestsTotal)
	prometheus.MustRegister(httpRequestDuration)
	prometheus.MustRegister(httpInFlightRequests)
	prometheus.MustRegister(httpRequestSize)
	prometheus.MustRegister(httpResponseSize)
}

func main() {
	flag.Parse()

	// Load departments (you'll need to implement loadDepartments())
	d := loadDepartments()
	departments = &d
	log.Printf("Loaded %d departments\n", len(*departments))

	s := &foiaServer{
		db: createDB(),
	}

	// Wrap the mux with the dynamic label middleware
	mux := s.CreateMux()
	// Create a dynamic label middleware
	dynamicLabelMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Dynamically set labels based on the request
			path := r.URL.Path // Capture the request path
			method := r.Method // Capture the request method

			// Wrap the original handler with dynamic labels
			handler := promhttp.InstrumentHandlerInFlight(httpInFlightRequests,
				promhttp.InstrumentHandlerDuration(httpRequestDuration.MustCurryWith(prometheus.Labels{"path": path, "method": method}),
					promhttp.InstrumentHandlerCounter(httpRequestsTotal.MustCurryWith(prometheus.Labels{"path": path, "method": method}),
						promhttp.InstrumentHandlerRequestSize(httpRequestSize.MustCurryWith(prometheus.Labels{"path": path, "method": method}),
							promhttp.InstrumentHandlerResponseSize(httpResponseSize.MustCurryWith(prometheus.Labels{"path": path, "method": method}),
								next)))))

			// Serve using the dynamically labeled handler
			handler.ServeHTTP(w, r)
		})
	}
	withPrometheus := dynamicLabelMiddleware(mux)
	withLogging := LoggingMiddleware(withPrometheus)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errChan := make(chan error, 1)

	mainServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: withLogging,
	}

	metricsMux := http.NewServeMux()
	tsweb.Debugger(metricsMux)
	promServer := &http.Server{
		Handler: metricsMux,
	}

	var metricsListener net.Listener
	if *runAsTSNet {
		if *tsnetDir == "" {
			log.Fatalf("must specify --tsnet-dir with --tsnet")
		}
		var err error
		s := tsnet.Server{
			Hostname: "sunshine",
			AuthKey:  os.Getenv("TS_AUTHKEY"),
			Logf:     log.Printf,
			Dir:      *tsnetDir,
		}
		metricsListener, err = s.Listen("tcp", ":80")
		log.Println("Starting Prometheus server on port 80 tsnet")
		if err != nil {
			log.Fatalf("Failed to listen on port 80: %v", err)
		}
	} else {
		var err error
		log.Println("Starting Prometheus server on port 8081")
		metricsListener, err = net.Listen("tcp", "localhost:8081")
		if err != nil {
			log.Fatalf("Failed to listen on port 8081: %v", err)
		}
	}
	// Start main server
	go func() {
		log.Printf("Starting server on port %d\n", *port)
		errChan <- mainServer.ListenAndServe()
	}()

	// Start Prometheus server
	go func() {
		errChan <- promServer.Serve(metricsListener)
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	log.Println("Shutting down servers...")

	// Create a timeout context for shutdown
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	// Shutdown both servers
	if err := mainServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("Main server shutdown error: %v\n", err)
	}
	if err := promServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("Prometheus server shutdown error: %v\n", err)
	}

	// Wait for server goroutines to exit
	select {
	case err := <-errChan:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("Server error: %v\n", err)
		}
	case <-shutdownCtx.Done():
		log.Println("Shutdown timeout")
	}

	log.Println("Servers successfully shut down")
}

func (s *foiaServer) indexHandler(w http.ResponseWriter, r *http.Request) {
	departments := make([]Department, 0)
	rows, err := s.db.Query(`
		SELECT name,
			name_slug,
			email,
			coalesce(contact_name, '') as contact_name,
			coalesce(notes, '') as notes,
			coalesce(url, '') as url
		FROM departments
		ORDER BY name ASC`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var d Department
		err := rows.Scan(&d.Name, &d.NameSlug, &d.Email, &d.ContactName, &d.Notes, &d.URL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		departments = append(departments, d)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	safeRender(w, indexTemplate, struct{ Departments []Department }{departments})
}

func (s *foiaServer) emailTemplateHandler(w http.ResponseWriter, r *http.Request) {
	safeRender(w, emailTemplateTemplate, struct {
		EmailBody string
	}{""})
}

func (s *foiaServer) departmentHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing department ID", http.StatusBadRequest)
		return
	}

	var department Department
	row := s.db.QueryRow(`SELECT name, name_slug, email, coalesce(contact_name, '') as contact_name, coalesce(notes, '') as notes, coalesce(url, '') as url FROM departments WHERE name_slug = ?`, id)
	if err := row.Scan(&department.Name, &department.NameSlug, &department.Email, &department.ContactName, &department.Notes, &department.URL); err != nil {
		http.Error(w, fmt.Sprintf("Department not found: %v", err), http.StatusNotFound)
		return
	}

	safeRender(w, departmentTemplate, struct {
		Department Department
		EmailBody  string
	}{department, "This is the email body"})
}

func (s *foiaServer) searchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var query struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
		http.Error(w, fmt.Sprintf("unable to decode body: %v", err.Error()), http.StatusBadRequest)
		return
	}

	if query.Query == "" {
		http.Error(w, "Missing query", http.StatusBadRequest)
		return

	}

	rows, err := s.db.Query(`SELECT name, name_slug, email FROM departments WHERE departments MATCH ?`, query.Query)
	if err != nil {
		http.Error(w, "Error querying department data", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type searchResult struct {
		Name     string `json:"name"`
		NameSlug string `json:"name_slug"`
		Email    string `json:"email"`
	}
	results := make([]searchResult, 0)

	for rows.Next() {
		r := searchResult{}
		if err := rows.Scan(&r.Name, &r.NameSlug, &r.Email); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func (s *foiaServer) listHandler(w http.ResponseWriter, r *http.Request) {
	safeRender(w, listTemplate, struct{ Departments map[string]Department }{loadDepartments()})
}

func safeRender(w http.ResponseWriter, tmpl *template.Template, data interface{}) {
	b := new(bytes.Buffer)
	if err := tmpl.ExecuteTemplate(b, "base.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	e := etag.Generate(b.Bytes(), true)
	w.Header().Set("ETag", e)
	w.Write(b.Bytes())
}

// lowercase, no space, sub all non alphanum with dash
func slugify(s string) string {
	s = strings.ToLower(s)
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		} else {
			return '-'
		}
	}, s)
}
