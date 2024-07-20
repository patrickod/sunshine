package main

import (
	"bytes"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/hhsnopek/etag"
	_ "github.com/mattn/go-sqlite3"
)

var (
	port = flag.Int("port", 8080, "http port to listen on")
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
	mux.HandleFunc("GET /", s.indexHandler)
	mux.HandleFunc("GET /list", s.listHandler)
	mux.HandleFunc("GET /email-template", s.emailTemplateHandler)
	mux.HandleFunc("GET /department/{id}", s.departmentHandler)
	mux.HandleFunc("POST /search", s.searchHandler)
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))
	return mux
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

	log.Printf("Starting server on port %d\n", *port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), LoggingMiddleware(s.CreateMux())); err != nil {
		if err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %s\n", err)
		}
	}
}

func (s *foiaServer) indexHandler(w http.ResponseWriter, r *http.Request) {
	safeRender(w, indexTemplate, departments)
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
