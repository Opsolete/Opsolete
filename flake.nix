{
  description = "Opsolete - from forms to infrastructure";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = {
    self,
    nixpkgs,
  }: let
    forEachSystem = nixpkgs.lib.genAttrs ["x86_64-linux" "aarch64-linux" "aarch64-darwin"];
    pkgsFor = system: nixpkgs.legacyPackages.${system};
  in {
    packages = forEachSystem (system: let
      pkgs = pkgsFor system;
    in {
      default = pkgs.buildGoModule {
        pname = "opsolete";
        version = "0.1.0";
        src = self;
        vendorHash = "sha256-Dj2gNDalPZy7nQ36TxDxeyR1YJKLzU+ZljZddhafcG0="; # update after go.mod changes
        subPackages = ["cmd/opsolete"];
        nativeBuildInputs = [pkgs.makeWrapper];
        # DB tests skip themselves without TEST_DATABASE_URL; the rest need the catalog from src.
        postInstall = ''
          mkdir -p $out/share/opsolete
          cp -r catalog $out/share/opsolete/
          wrapProgram $out/bin/opsolete --set-default TEMPLATES_DIR $out/share/opsolete/catalog
        '';
      };
    });

    devShells = forEachSystem (system: let
      # terraform is BSL-licensed (unfree in nixpkgs); allow just that one.
      pkgs = import nixpkgs {
        inherit system;
        config.allowUnfreePredicate = p: nixpkgs.lib.getName p == "terraform";
      };
      # postgres runs project-local on a unix socket in .devdata/ (no port clash); see justfile.
    in {
      default = pkgs.mkShell {
        packages = with pkgs; [go gopls gotools postgresql_17 just curl docker-client terraform infracost];
        shellHook = ''
          export PGHOST="$PWD/.devdata"
          export PGDATA="$PGHOST/pg"
          export PGDATABASE=opsolete
          mkdir -p "$PGHOST"
          export DATABASE_URL="postgres:///opsolete?host=$PGHOST"
          export TEST_DATABASE_URL="postgres:///opsolete_test?host=$PGHOST"
          export TEMPLATE_EDITOR=1 # dev only; see internal/config
          export AUTH_DEV_USER=dev@localhost ADMIN_EMAILS=dev@localhost # dev only: no OIDC login locally
          export STATE_STORE="file://$PGHOST/state-store?create_dir=1&metadata=skip" # workspace state files
          export PUBLIC_URL=http://opsolete.localhost:8080 # registry host needs a dot
          export SECRET_KEY=dev-only-secret-key-not-for-production # encrypts VCS tokens; dev only
          echo "opsolete: run 'just' to list tasks ('just run', 'just check')"
        '';
      };
    });

    formatter = forEachSystem (system: (pkgsFor system).alejandra);
  };
}
