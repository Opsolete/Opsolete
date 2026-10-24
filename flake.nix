{
  description = "Form2Stack - from forms to infrastructure";

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
        pname = "form2stack";
        version = "0.1.0";
        src = self;
        vendorHash = "sha256-8VFCHlcnwVK+l3PbB24rv5XgknPjq+hwNKGzHMD2YQg="; # update after go.mod changes
        subPackages = ["cmd/form2stack"];
        nativeBuildInputs = [pkgs.makeWrapper];
        # DB tests skip themselves without TEST_DATABASE_URL; the rest need the catalog from src.
        postInstall = ''
          mkdir -p $out/share/form2stack
          cp -r catalog $out/share/form2stack/
          wrapProgram $out/bin/form2stack --set-default TEMPLATES_DIR $out/share/form2stack/catalog
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
        packages = with pkgs; [go gopls gotools postgresql_17 just curl docker-client terraform];
        shellHook = ''
          export PGHOST="$PWD/.devdata"
          export PGDATA="$PGHOST/pg"
          export PGDATABASE=form2stack
          mkdir -p "$PGHOST"
          export DATABASE_URL="postgres:///form2stack?host=$PGHOST"
          export TEST_DATABASE_URL="postgres:///form2stack_test?host=$PGHOST"
          export TEMPLATE_EDITOR=1 # dev only; see internal/config
          export AUTH_DEV_USER=dev@localhost ADMIN_EMAILS=dev@localhost # dev only: no OIDC login locally
          export STATE_STORE="file://$PGHOST/state-store?create_dir=1&metadata=skip" # workspace state files
          export PUBLIC_URL=http://form2stack.localhost:8080 # registry host needs a dot
          export SECRET_KEY=dev-only-secret-key-not-for-production # encrypts VCS tokens; dev only
          echo "form2stack: run 'just' to list tasks ('just run', 'just check')"
        '';
      };
    });

    formatter = forEachSystem (system: (pkgsFor system).alejandra);
  };
}
