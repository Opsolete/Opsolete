{
  description = "Opsolete - from forms to infrastructure";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = {
    self,
    nixpkgs,
  }: let
    forEachSystem = nixpkgs.lib.genAttrs ["x86_64-linux" "aarch64-linux" "aarch64-darwin"];
    pkgsFor = system: nixpkgs.legacyPackages.${system};
    # HashiCorp's release binary pinned by checksum, in the images, CI and the dev shell alike: nixpkgs' terraform
    # lags behind. The version is .terraform-version (also read by CI and `just test-linux`); update the checksums with it.
    terraformFor = pkgs: system: let
      tfVersion = pkgs.lib.trim (builtins.readFile ./.terraform-version);
      platform =
        {
          x86_64-linux = "linux_amd64";
          aarch64-linux = "linux_arm64";
          aarch64-darwin = "darwin_arm64";
        }.${
          system
        };
    in
      pkgs.runCommand "terraform-${tfVersion}" {
        src = pkgs.fetchurl {
          url = "https://releases.hashicorp.com/terraform/${tfVersion}/terraform_${tfVersion}_${platform}.zip";
          sha256 =
            {
              linux_amd64 = "4638628058e71a55212b1c563712eac03b902128f40740f3dc8174df12135061";
              linux_arm64 = "988e950a1c23058093b0e66ab2d5fb53fc18345ecfbd5871acf51dbac9325edd";
              darwin_arm64 = "b1ecb3fe0d0a0290c1ab6f83d967d43f25da05d0b671ee0fb355252268dce95a";
            }.${
              platform
            };
        };
        nativeBuildInputs = [pkgs.unzip];
      } "mkdir -p $out/bin && unzip -q $src terraform -d $out/bin";
  in {
    packages = forEachSystem (system: let
      pkgs = pkgsFor system;
      # nixpkgs' go, prebuilt on cache.nixos.org: keep flake.lock fresh so it has the stdlib security fixes.
      opsolete = pkgs.buildGoModule {
        pname = "opsolete";
        version = "0.6.0"; # x-release-please-version
        src = self;
        vendorHash = "sha256-uUu24RaF4aCmOBZGeu6p7C9WiiLjuCgXeVbyzVVnVwE="; # update after go.mod changes
        subPackages = ["cmd/opsolete"];
        # DB tests skip themselves without TEST_DATABASE_URL; the rest use catalog/ from src. The package
        # ships no templates: they come from a Git repository (Settings → Templates), or TEMPLATES_DIR.
      };

      # Container images (linux only): `nix build .#image-app`, then `docker load < result`.
      arch =
        {
          x86_64-linux = "amd64";
          aarch64-linux = "arm64";
        }.${
          system
        };
      terraform = terraformFor pkgs system;
      # Cost estimation; only used when the runner has OPS_INFRACOST_API_KEY.
      infracost = pkgs.runCommand "infracost-0.10.47" {
        src = pkgs.fetchurl {
          url = "https://github.com/infracost/infracost/releases/download/v0.10.47/infracost-linux-${arch}.tar.gz";
          sha256 =
            {
              amd64 = "df098d5565b78cc23de57e1dac9f2a02491691322cbd690dca9b197e3e02fe93";
              arm64 = "1db33dddcac989f39687fa9bf00ed9d8cce4268450578848e5bf6287ad8a7b44";
            }.${
              arch
            };
        };
      } "tar -xzf $src && install -Dm755 infracost-linux-${arch} $out/bin/infracost";
      labels."org.opencontainers.image.source" = "https://github.com/Opsolete/Opsolete"; # links the ghcr package to the repo
      rootfs = paths:
        pkgs.buildEnv {
          name = "rootfs";
          paths = paths ++ [pkgs.dockerTools.caCertificates];
          pathsToLink = ["/bin" "/etc"];
        };
    in
      {default = opsolete;}
      // pkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
        # Web UI, registry, runner API.
        image-app = pkgs.dockerTools.buildImage {
          name = "opsolete-app";
          tag = "latest";
          copyToRoot = rootfs [opsolete];
          extraCommands = "mkdir -m 1777 tmp";
          config = {
            Entrypoint = ["/bin/opsolete"];
            User = "65532:65532";
            Env = ["ADDR=:8080" "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"];
            ExposedPorts."8080/tcp" = {};
            Labels = labels;
          };
        };

        # One job at a time per container; scale with replicas. git: git:: module sources; sh and
        # coreutils: local-exec provisioners. State never stays here: it lives on the server's state store.
        image-runner = pkgs.dockerTools.buildImage {
          name = "opsolete-runner";
          tag = "latest";
          copyToRoot = rootfs [
            opsolete
            terraform
            infracost
            pkgs.git
            pkgs.openssh
            pkgs.coreutils
            pkgs.dockerTools.binSh
          ];
          # buildImage writes everything as root; world-writable (sticky) dirs instead of a chown, since only uid 10001 runs here.
          # Provider plugins are reused across jobs (mount a volume on the cache to keep them across restarts).
          # passwd/group (ssh refuses to run for a uid without a passwd entry) are written as plain files, not
          # symlinks into /nix/store (as fakeNss would give): containerd reads them from the host side to resolve
          # the user's groups and rejects symlinks that leave the rootfs ("path escapes from parent").
          extraCommands = ''
            mkdir -m 1777 tmp && mkdir -p home/runner/.terraform.d/plugin-cache && chmod -R 1777 home
            if [ -L etc ]; then t=$(readlink -f etc); rm etc; mkdir etc; cp -RP "$t"/. etc/; fi
            chmod u+w etc
            printf '%s\n' 'root:x:0:0::/root:/bin/sh' 'runner:x:10001:10001::/home/runner:/bin/sh' > etc/passwd
            printf '%s\n' 'root:x:0:' 'runner:x:10001:' > etc/group
          '';
          config = {
            Entrypoint = ["/bin/opsolete" "runner"];
            User = "10001:10001";
            Env = [
              "PATH=/bin"
              "HOME=/home/runner"
              "TF_PLUGIN_CACHE_DIR=/home/runner/.terraform.d/plugin-cache"
              "TF_IN_AUTOMATION=1"
              "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"
            ];
            Labels = labels;
          };
        };
      });

    devShells = forEachSystem (system: let
      pkgs = pkgsFor system;
      terraform = terraformFor pkgs system; # shadows pkgs.terraform in the list below
      # postgres runs project-local on a unix socket in .devdata/ (no port clash); see justfile.
    in {
      default = pkgs.mkShell {
        packages = with pkgs; [go gopls gotools sqlc postgresql_17 just curl docker-client terraform infracost hugo];
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
          export RUNNER_TOKEN=dev-only-runner-token-not-for-production # `just runner` passes it on; dev only
          echo "opsolete: run 'just' to list tasks ('just run', 'just check')"
        '';
      };
    });

    # services.opsolete and services.opsolete.runner; see nix/module.nix.
    nixosModules.default = import ./nix/module.nix self;

    # NixOS VM test of the module (needs a Linux builder with kvm).
    checks.x86_64-linux.nixos = (pkgsFor "x86_64-linux").testers.runNixOSTest (import ./nix/test.nix self);

    formatter = forEachSystem (system: (pkgsFor system).alejandra);
  };
}
