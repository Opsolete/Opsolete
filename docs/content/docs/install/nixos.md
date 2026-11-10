---
title: NixOS
weight: 3
---

Add this repository as a flake input and import its module. It creates a local PostgreSQL database by default, and passes secrets in from files (here from [sops-nix](https://github.com/Mic92/sops-nix)), so they never reach the Nix store:

```nix
# flake.nix
{
  inputs.opsolete.url = "github:Opsolete/Opsolete";
  outputs = { nixpkgs, opsolete, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [ opsolete.nixosModules.default ./configuration.nix ];
    };
  };
}
```

```nix
# configuration.nix
services.opsolete = {
  enable = true;
  publicUrl = "https://opsolete.example.com";
  secretKeyFile = config.sops.secrets.opsolete-secret-key.path;
  runnerTokenFile = config.sops.secrets.opsolete-runner-token.path;
  stateStore = "azblob://tfstate"; # default: files under /var/lib/opsolete
  oidc = { issuer = "https://login.example.com"; clientId = "opsolete"; clientSecretFile = config.sops.secrets.opsolete-oidc.path; };
  adminEmails = [ "you@example.com" ];
  runner.enable = true; # a runner on the same host; terraform is unfree (allowUnfree), or set runner.terraformPackage = pkgs.opentofu
};
```

The server listens on `127.0.0.1:8080`: put a reverse proxy with TLS in front of it, for example nginx, at `publicUrl`.

## Options and secrets

[nix/module.nix](https://github.com/Opsolete/Opsolete/blob/master/nix/module.nix) lists every option; the [option reference]({{< relref "/docs/config/reference" >}}) maps them to the env vars. Settings without an option (`METRICS_ADDR`, `SECRET_KEY_PREVIOUS`, cloud credentials) go in `environment`, secret ones in `secretFiles`. Secrets are `*File` options taking a path (e.g. `config.sops.secrets.<name>.path`), passed in through systemd credentials, so they never reach the Nix store. By default it creates a local Postgres database with peer auth.

`opsolete-admin` runs any subcommand with the server's settings: create registry tokens with `sudo opsolete-admin token <name>`, rotate the key with `sudo opsolete-admin rotate-key`. A runner's `tokenFile` defaults to `runnerTokenFile`; a runner on another host sets `runner.tokenFile` to the same secret. The runner runs in its own PID namespace, so jobs see no other processes of the host. It reads its secret files as root and then drops to its user, so module code (which runs as that user) can't read them. Test: `nix build .#checks.x86_64-linux.nixos` (NixOS VM, needs kvm).
