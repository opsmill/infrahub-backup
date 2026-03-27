{
  description = "infrahub-backup - Backup/restore and task management tools for Infrahub";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        version =
          if self ? shortRev then self.shortRev
          else if self ? dirtyShortRev then self.dirtyShortRev
          else "dev";

        # Shared build configuration for both binaries.
        # The neo4j watchdog must be pre-built because both packages
        # import src/internal/app which has //go:embed directives
        # referencing the watchdog binaries.
        commonAttrs = {
          inherit version;
          src = ./.;

          vendorHash = "sha256-W7KHeb9vue1Z9pCi00q174yHi0RtbVLjYlSGdVSRo7s=";

          # Don't run preBuild (watchdog compilation) in the go-modules
          # derivation — it only needs to fetch/vendor dependencies.
          overrideModAttrs = finalAttrs: prevAttrs: {
            preBuild = "";
          };

          env.CGO_ENABLED = 0;

          ldflags = [
            "-s" "-w"
            "-X main.version=${version}"
          ];

          preBuild = ''
            # Build embedded neo4j watchdog binaries required by //go:embed
            GOOS=linux GOARCH=arm64 go build -ldflags "-s -w" \
              -o src/internal/app/embedded/neo4jwatchdog/neo4j_watchdog_linux_arm64 \
              ./tools/neo4jwatchdog
            GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" \
              -o src/internal/app/embedded/neo4jwatchdog/neo4j_watchdog_linux_amd64 \
              ./tools/neo4jwatchdog
          '';

          meta = with pkgs.lib; {
            homepage = "https://github.com/opsmill/infrahub-backup";
            license = licenses.asl20;
            maintainers = [ ];
          };
        };
      in
      {
        packages = {
          infrahub-backup = pkgs.buildGoModule (commonAttrs // {
            pname = "infrahub-backup";
            subPackages = [ "src/cmd/infrahub-backup" ];
            meta = commonAttrs.meta // {
              description = "Backup and restore tool for Infrahub instances";
              mainProgram = "infrahub-backup";
            };
          });

          infrahub-taskmanager = pkgs.buildGoModule (commonAttrs // {
            pname = "infrahub-taskmanager";
            subPackages = [ "src/cmd/infrahub-taskmanager" ];
            meta = commonAttrs.meta // {
              description = "Task manager maintenance tool for Infrahub instances";
              mainProgram = "infrahub-taskmanager";
            };
          });

          default = pkgs.symlinkJoin {
            name = "infrahub-ops-cli-${version}";
            paths = [
              self.packages.${system}.infrahub-backup
              self.packages.${system}.infrahub-taskmanager
            ];
          };
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            golangci-lint
            gopls
          ];
        };
      });
}
