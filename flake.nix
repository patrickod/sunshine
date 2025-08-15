{
  description = "SunshineSF - San Francisco FOIA Request Tool";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        sunshineSF = pkgs.buildGoModule {
          pname = "sunshine-sf";
          version = "0.1.0";

          src = ./.;

          vendorHash = "sha256-NnZ0sXuuA8D/6hS0vEXtaikaIeClwX+/8nEquA/n20I=";

          ldflags = [ "-s" "-w" ];

          meta = with pkgs.lib; {
            description = "San Francisco FOIA Request Tool";
            homepage = "https://github.com/patrickod/sunshine";
            license = licenses.mit;
            maintainers = [ ];
          };
        };
      in
      {
        packages = {
          default = sunshineSF;
          sunshine-sf = sunshineSF;
        };

        apps = {
          default = flake-utils.lib.mkApp {
            drv = sunshineSF;
          };
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            sqlite
          ];
        };
      }
    ) // {
      nixosModules.default = { config, lib, pkgs, ... }:
        with lib;
        let
          cfg = config.services.sunshine-sf;
          pkg = self.packages.${pkgs.system}.sunshine-sf;
        in
        {
          options.services.sunshine-sf = {
            enable = mkEnableOption "SunshineSF FOIA request tool";

            port = mkOption {
              type = types.port;
              default = 8080;
              description = "Port to listen on for HTTP requests";
            };

            metricsPort = mkOption {
              type = types.port;
              default = 8081;
              description = "Port to listen on for Prometheus metrics";
            };

            listenAddress = mkOption {
              type = types.str;
              default = "127.0.0.1";
              description = "Address to listen on";
            };

            dataDir = mkOption {
              type = types.path;
              default = "/var/lib/sunshine-sf";
              description = "Directory to store application data";
            };

            user = mkOption {
              type = types.str;
              default = "sunshine-sf";
              description = "User to run the service as";
            };

            group = mkOption {
              type = types.str;
              default = "sunshine-sf";
              description = "Group to run the service as";
            };
          };

          config = mkIf cfg.enable {
            users.users.${cfg.user} = {
              isSystemUser = true;
              group = cfg.group;
              home = cfg.dataDir;
              createHome = true;
              homeMode = "750";
            };

            users.groups.${cfg.group} = {};

            systemd.services.sunshine-sf = {
              description = "SunshineSF FOIA Request Tool";
              wantedBy = [ "multi-user.target" ];
              after = [ "network.target" ];

              serviceConfig = {
                Type = "simple";
                User = cfg.user;
                Group = cfg.group;
                WorkingDirectory = cfg.dataDir;
                ExecStart = "${pkg}/bin/sunshine -port=${toString cfg.port}";
                Restart = "always";
                RestartSec = "10s";

                # Security settings
                NoNewPrivileges = true;
                ProtectSystem = "strict";
                ProtectHome = true;
                PrivateTmp = true;
                PrivateDevices = true;
                ProtectHostname = true;
                ProtectClock = true;
                ProtectKernelTunables = true;
                ProtectKernelModules = true;
                ProtectKernelLogs = true;
                ProtectControlGroups = true;
                RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
                RestrictNamespaces = true;
                LockPersonality = true;
                MemoryDenyWriteExecute = true;
                RestrictRealtime = true;
                RestrictSUIDSGID = true;
                RemoveIPC = true;

                # Allow writing to data directory
                ReadWritePaths = [ cfg.dataDir ];
              };

              environment = {
                HOME = cfg.dataDir;
              };
            };

            # Ensure the data directory has correct permissions
            systemd.tmpfiles.rules = [
              "d '${cfg.dataDir}' 0750 ${cfg.user} ${cfg.group} - -"
            ];
          };
        };
    };
}
