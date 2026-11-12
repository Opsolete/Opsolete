# NixOS module: services.opsolete (server) and services.opsolete.runner.
# Secrets are *File options holding a path (e.g. config.sops.secrets.<name>.path). They reach the
# server via systemd LoadCredential, the runner via a root launcher, so they never enter the Nix store
# or the unit's Environment=, and the files may stay root-owned (sops-nix's default).
self: {
  config,
  lib,
  pkgs,
  ...
}: let
  inherit (lib) mkOption mkEnableOption mkIf optional optionalAttrs types;
  cfg = config.services.opsolete;
  rcfg = cfg.runner;

  nullOr = t: default: description:
    mkOption {
      type = types.nullOr t;
      inherit default description;
    };
  secretFile = description:
    mkOption {
      type = types.nullOr types.str; # str, not path: a ./literal would be copied into the world-readable store
      default = null;
      example = "/run/secrets/opsolete-secret-key";
      inherit description;
    };
  secretFiles = description:
    mkOption {
      type = types.attrsOf types.str;
      default = {};
      example = {AZURE_STORAGE_KEY = "/run/secrets/azure-storage-key";};
      inherit description;
    };

  opsolete = lib.getExe' cfg.package "opsolete";
  # `$(<f)` drops the trailing newline that sops values and editors leave.
  exportSecrets = pathOf: secrets: lib.concatLines (lib.mapAttrsToList (k: f: ''export ${k}="$(< "${pathOf k f}")"'') secrets);
  launcher = name: secrets: args:
    pkgs.writeShellScript name ''
      set -euo pipefail
      ${exportSecrets (k: _: "$CREDENTIALS_DIRECTORY/${k}") secrets}
      exec ${opsolete} ${args}
    '';
  credentials = lib.mapAttrsToList (k: f: "${k}:${f}");
  # The runner reads its secret files as root, then drops to its user. Not LoadCredential: job code runs
  # as that user and could read /run/credentials/… (the env is hidden from jobs, the directory isn't).
  runnerLauncher = pkgs.writeShellScript "opsolete-runner-start" ''
    set -euo pipefail
    ${exportSecrets (_: f: f) runnerSecrets}
    exec ${pkgs.util-linux}/bin/setpriv --reuid=opsolete-runner --regid=opsolete-runner --clear-groups -- ${opsolete} runner
  '';
  dropNulls = lib.filterAttrs (_: v: v != null);
  joinOrNull = l:
    if l == []
    then null
    else lib.concatStringsSep "," l;

  serverEnv =
    dropNulls {
      ADDR = "${cfg.listenAddress}:${toString cfg.port}";
      PUBLIC_URL = cfg.publicUrl;
      DATABASE_URL =
        if cfg.database.createLocally
        then "postgres:///opsolete?host=/run/postgresql"
        else cfg.database.url;
      STATE_STORE = cfg.stateStore;
      TEMPLATES_DIR = cfg.templatesDir;
      TEMPLATE_EDITOR =
        if cfg.templateEditor
        then "1"
        else null;
      DEPLOYMENTS_ENABLED = lib.boolToString cfg.deployments.enable;
      REGISTRY_ENABLED = lib.boolToString cfg.registry.enable;
      ORGANIZATION = cfg.organization;
      STALE_LOCK_TIMEOUT = cfg.staleLockTimeout;
      RUN_LOG_RETENTION_DAYS = toString cfg.runLogRetentionDays;
      ADMIN_EMAILS = joinOrNull cfg.adminEmails;
      ADMIN_GROUPS = joinOrNull cfg.adminGroups;
      OIDC_ISSUER = cfg.oidc.issuer;
      OIDC_CLIENT_ID = cfg.oidc.clientId;
      AUTH_DEV_USER = cfg.devUser;
      AUTH_DEV_GROUPS = joinOrNull cfg.devGroups;
    }
    // cfg.environment;
  serverSecrets =
    cfg.secretFiles
    // dropNulls {
      SECRET_KEY = cfg.secretKeyFile;
      OIDC_CLIENT_SECRET = cfg.oidc.clientSecretFile;
      DATABASE_URL = cfg.database.urlFile;
    };

  # Registers this host's runner (runnerTokenFile) before the server starts: its own credential, not the server's env.
  runnerName =
    if rcfg.name != null
    then rcfg.name
    else config.networking.hostName;
  registerRunner = pkgs.writeShellScript "opsolete-register-runner" ''
    set -euo pipefail
    ${exportSecrets (k: _: "$CREDENTIALS_DIRECTORY/${k}") serverSecrets}
    exec ${opsolete} runner-token -from-stdin -pool ${lib.escapeShellArg rcfg.pool} ${lib.escapeShellArg runnerName} < "$CREDENTIALS_DIRECTORY/runner-token"
  '';

  stateDir = "/var/lib/opsolete-runner";
  runnerEnv =
    dropNulls {
      OPS_SERVER_URL = rcfg.serverUrl;
      OPS_RUNNER_NAME = rcfg.name;
      OPS_JOB_TIMEOUT = rcfg.jobTimeout;
      OPS_JOB_GRACE = rcfg.jobGrace;
      OPS_TERRAFORM = lib.getExe rcfg.terraformPackage;
      OPS_INFRACOST =
        if rcfg.infracost.apiKeyFile != null
        then lib.getExe rcfg.infracost.package
        else null;
      HOME = stateDir; # each job still gets its own HOME
      TF_PLUGIN_CACHE_DIR = "${stateDir}/plugin-cache";
      TF_IN_AUTOMATION = "1";
    }
    // rcfg.environment;
  runnerSecrets =
    rcfg.secretFiles
    // dropNulls {
      OPS_RUNNER_TOKEN = rcfg.tokenFile;
      OPS_INFRACOST_API_KEY = rcfg.infracost.apiKeyFile;
    };

  # `opsolete-admin token <name>` (and any other subcommand) with the server's config.
  admin = pkgs.writeShellScriptBin "opsolete-admin" ''
    set -euo pipefail
    [ "$(id -u)" = 0 ] || { echo "opsolete-admin: run as root (it reads the secret files)" >&2; exit 1; }
    ${lib.concatLines (lib.mapAttrsToList (k: v: "export ${k}=${lib.escapeShellArg v}") serverEnv)}
    ${exportSecrets (_: f: f) serverSecrets}
    cd /
    # runuser keeps the environment; as user opsolete, postgres peer auth matches the local role
    exec ${pkgs.util-linux}/bin/runuser -u opsolete -- ${opsolete} "$@"
  '';

  hardening = {
    NoNewPrivileges = true;
    PrivateTmp = true;
    PrivateDevices = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    ProtectClock = true;
    ProtectControlGroups = true;
    ProtectHostname = true;
    ProtectKernelLogs = true;
    ProtectKernelModules = true;
    ProtectKernelTunables = true;
    RestrictAddressFamilies = ["AF_UNIX" "AF_INET" "AF_INET6"];
    RestrictNamespaces = true;
    RestrictRealtime = true;
    RestrictSUIDSGID = true;
    LockPersonality = true;
    SystemCallArchitectures = "native";
    UMask = "0077";
    Restart = "on-failure";
    RestartSec = 10;
  };
in {
  options.services.opsolete = {
    enable = mkEnableOption "the Opsolete server (web UI, module registry, runner API)";
    package = mkOption {
      type = types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "opsolete.packages.\${system}.default";
      description = "Opsolete package; also used by the runner and opsolete-admin.";
    };
    listenAddress = mkOption {
      type = types.str;
      default = "127.0.0.1";
      description = "ADDR host. Use brackets for IPv6, e.g. [::1].";
    };
    port = mkOption {
      type = types.port;
      default = 8080;
      description = "ADDR port.";
    };
    openFirewall = mkOption {
      type = types.bool;
      default = false;
      description = "Open `port` in the firewall.";
    };
    publicUrl = mkOption {
      type = types.str;
      example = "https://opsolete.example.com";
      description = "PUBLIC_URL: how browsers and Terraform reach the app. Its host (needs a dot) is the registry hostname; the OIDC callback is PUBLIC_URL/auth/callback.";
    };
    secretKeyFile = mkOption {
      type = types.str;
      example = "/run/secrets/opsolete-secret-key";
      description = "File with SECRET_KEY (32+ characters, e.g. `openssl rand -base64 32`). Encrypts state, plans, variables and VCS tokens and signs the audit log: back it up, losing it loses all state.";
    };
    runnerTokenFile = secretFile "File with the token of this host's runner (32+ characters, e.g. `openssl rand -hex 32`): the server registers it at start as runner `runner.name` (default: the hostname) in pool `runner.pool`, and the runner's tokenFile defaults to it. Runners elsewhere get their own token: `opsolete-admin runner-token <name>`, or Settings → Runners.";
    database = {
      createLocally = mkOption {
        type = types.bool;
        default = true;
        description = "Create a local PostgreSQL database and role `opsolete`, reached over the unix socket with peer auth (no password).";
      };
      url = nullOr types.str null "DATABASE_URL without a password (createLocally = false). Use urlFile when it holds credentials.";
      urlFile = secretFile "File with DATABASE_URL, for URLs with a password (createLocally = false).";
    };
    stateStore = mkOption {
      type = types.str;
      # no_tmp_dir: PrivateTmp puts /tmp on another mount, so fileblob's rename from /tmp fails (EXDEV)
      default = "file:///var/lib/opsolete/state?create_dir=1&metadata=skip&no_tmp_dir=1";
      example = "azblob://tfstate";
      description = "STATE_STORE (Go CDK blob URL). Cloud credentials go in `secretFiles` (secrets) or `environment` (e.g. AZURE_STORAGE_ACCOUNT). A file:// URL must be under /var/lib/opsolete (the only writable path) and needs `no_tmp_dir=1`.";
    };
    templatesDir = nullOr types.str null "TEMPLATES_DIR, a local template folder. null: none; keep the templates in a Git repository instead (Settings → Templates).";
    templateEditor = mkOption {
      type = types.bool;
      default = false;
      description = "TEMPLATE_EDITOR (development): the admin template builder writes into templatesDir (must then be a directory writable by user opsolete), and module uploads are on. Not needed for a Git template catalog, whose editor is set up in Settings → Templates.";
    };
    deployments.enable = mkOption {
      type = types.bool;
      default = true;
      description = "DEPLOYMENTS_ENABLED: templates, workspaces, runs, runner API and state backend.";
    };
    registry.enable = mkOption {
      type = types.bool;
      default = true;
      description = "REGISTRY_ENABLED: module registry pages and protocol.";
    };
    organization = mkOption {
      type = types.str;
      default = "opsolete";
      description = "ORGANIZATION: the one organization Terraform's cloud block may name.";
    };
    staleLockTimeout = mkOption {
      type = types.str;
      default = "15m";
      description = "STALE_LOCK_TIMEOUT (Go duration).";
    };
    runLogRetentionDays = mkOption {
      type = types.ints.between 0 36500;
      default = 90;
      description = "RUN_LOG_RETENTION_DAYS: run logs are deleted this many days after the run ended; 0 keeps them forever.";
    };
    adminEmails = mkOption {
      type = types.listOf types.str;
      default = [];
      description = "ADMIN_EMAILS: emails (the IdP's email claim) that get the admin role.";
    };
    adminGroups = mkOption {
      type = types.listOf types.str;
      default = [];
      description = "ADMIN_GROUPS: IdP groups (the groups claim) that get the admin role.";
    };
    oidc = {
      issuer = nullOr types.str null "OIDC_ISSUER.";
      clientId = nullOr types.str null "OIDC_CLIENT_ID.";
      clientSecretFile = secretFile "File with OIDC_CLIENT_SECRET.";
    };
    devUser = nullOr types.str null "AUTH_DEV_USER: skip login, every visitor is this email. Development only.";
    devGroups = mkOption {
      type = types.listOf types.str;
      default = [];
      description = "AUTH_DEV_GROUPS: the dev user's groups.";
    };
    environment = mkOption {
      type = types.attrsOf types.str;
      default = {};
      description = "Extra non-secret environment variables (they end up in the Nix store).";
    };
    secretFiles = secretFiles "Extra secret environment variables: name -> file holding the value (e.g. a sops-nix secret path).";

    runner = {
      enable = mkEnableOption "an Opsolete runner (executes Terraform runs, one at a time)";
      serverUrl = mkOption {
        type = types.str;
        default = "http://${
          if lib.elem cfg.listenAddress ["" "0.0.0.0" "[::]"]
          then "127.0.0.1"
          else cfg.listenAddress
        }:${toString cfg.port}";
        defaultText = lib.literalExpression ''"http://''${listenAddress}:''${toString port}" (the local server)'';
        description = "OPS_SERVER_URL.";
      };
      tokenFile = mkOption {
        type = types.nullOr types.str;
        default = cfg.runnerTokenFile;
        defaultText = lib.literalExpression "config.services.opsolete.runnerTokenFile";
        example = "/run/secrets/opsolete-runner-token";
        description = "File with OPS_RUNNER_TOKEN: the runner's own token (runnerTokenFile on the server's host, else from `opsolete-admin runner-token <name>`).";
      };
      name = nullOr types.str null "OPS_RUNNER_NAME, and the name runnerTokenFile is registered with. null: the hostname.";
      pool = mkOption {
        type = types.str;
        default = "default";
        description = "The pool runnerTokenFile's runner is registered in: it gets the runs of projects that use this pool.";
      };
      jobTimeout = mkOption {
        type = types.str;
        default = "1h";
        description = "OPS_JOB_TIMEOUT (Go duration, also valid as a systemd time span: e.g. 1h, 90m): limit per run phase, then Terraform is interrupted. A stop waits for the current job up to this plus jobGrace plus 5 minutes.";
      };
      jobGrace = mkOption {
        type = types.str;
        default = "10m";
        description = "OPS_JOB_GRACE (Go duration, also valid as a systemd time span): after any interrupt (timeout, cancel, stop), how long Terraform may take to finish in-flight operations and save state before it is killed.";
      };
      terraformPackage = mkOption {
        type = types.package;
        default = pkgs.terraform; # unfree (BSL): needs allowUnfree or an allowUnfreePredicate
        defaultText = lib.literalExpression "pkgs.terraform";
        description = "Terraform for OPS_TERRAFORM.";
      };
      infracost = {
        apiKeyFile = secretFile "File with OPS_INFRACOST_API_KEY; enables cost estimation.";
        package = mkOption {
          type = types.package;
          default = pkgs.infracost;
          defaultText = lib.literalExpression "pkgs.infracost";
          description = "Infracost for OPS_INFRACOST.";
        };
      };
      extraPackages = mkOption {
        type = types.listOf types.package;
        default = [];
        description = "Extra tools on PATH for jobs (git, ssh and bash are always there), e.g. for local-exec.";
      };
      environment = mkOption {
        type = types.attrsOf types.str;
        default = {};
        description = "Extra non-secret environment variables. Jobs see them.";
      };
      secretFiles = secretFiles "Extra secret environment variables: name -> file. Only OPS_* are hidden from jobs; anything else here is visible to every run's module code.";
    };
  };

  config = lib.mkMerge [
    (mkIf cfg.enable {
      assertions = [
        {
          assertion =
            if cfg.database.createLocally
            then cfg.database.url == null && cfg.database.urlFile == null
            else (cfg.database.url == null) != (cfg.database.urlFile == null);
          message = "services.opsolete.database: set exactly one of url / urlFile, and only with createLocally = false";
        }
        {
          assertion = cfg.templateEditor -> cfg.templatesDir != null;
          message = "services.opsolete.templateEditor writes into templatesDir: set it to a writable directory";
        }
      ];

      users.users.opsolete = {
        isSystemUser = true;
        group = "opsolete";
      };
      users.groups.opsolete = {};
      environment.systemPackages = [admin];
      networking.firewall.allowedTCPPorts = optional cfg.openFirewall cfg.port;

      services.postgresql = mkIf cfg.database.createLocally {
        enable = true;
        ensureDatabases = ["opsolete"];
        ensureUsers = [
          {
            name = "opsolete";
            ensureDBOwnership = true;
          }
        ];
      };

      systemd.services.opsolete = {
        description = "Opsolete";
        wantedBy = ["multi-user.target"];
        wants = ["network-online.target"];
        after = ["network-online.target"] ++ optional cfg.database.createLocally "postgresql.target";
        requires = optional cfg.database.createLocally "postgresql.target";
        environment = serverEnv;
        serviceConfig =
          hardening
          // {
            ExecStartPre = optional (cfg.runnerTokenFile != null) registerRunner;
            ExecStart = launcher "opsolete-start" serverSecrets "";
            User = "opsolete";
            Group = "opsolete";
            StateDirectory = "opsolete";
            LoadCredential = credentials serverSecrets ++ optional (cfg.runnerTokenFile != null) "runner-token:${cfg.runnerTokenFile}";
            ReadWritePaths = optional cfg.templateEditor cfg.templatesDir;
            AmbientCapabilities = optional (cfg.port < 1024) "CAP_NET_BIND_SERVICE";
            CapabilityBoundingSet =
              if cfg.port < 1024
              then "CAP_NET_BIND_SERVICE"
              else "";
          };
      };
    })

    (mkIf rcfg.enable {
      assertions = [
        {
          assertion = rcfg.tokenFile != null;
          message = "services.opsolete.runner.tokenFile: set it, or services.opsolete.runnerTokenFile when the server runs on this host";
        }
      ];
      # its own user: module code runs as it, so it must not share the server's (database) identity
      users.users.opsolete-runner = {
        isSystemUser = true;
        group = "opsolete-runner";
      };
      users.groups.opsolete-runner = {};

      systemd.services.opsolete-runner = {
        description = "Opsolete runner";
        wantedBy = ["multi-user.target"];
        wants = ["network-online.target"];
        after = ["network-online.target"] ++ optional cfg.enable "opsolete.service";
        environment = runnerEnv;
        path = [pkgs.git pkgs.openssh pkgs.bash] ++ rcfg.extraPackages;
        serviceConfig =
          hardening
          // {
            # "!": started as root (User= still owns StateDirectory); runnerLauncher drops to it itself
            ExecStart = "!${runnerLauncher}";
            User = "opsolete-runner"; # not DynamicUser: that fails to start together with PrivatePIDs
            Group = "opsolete-runner";
            StateDirectory = ["opsolete-runner" "opsolete-runner/plugin-cache"];
            # own PID namespace: jobs see no other processes of the host (the runner kills what a job leaves running itself)
            PrivatePIDs = true;
            # for the launcher only (read secret files of any owner, drop to the user); gone after setpriv
            CapabilityBoundingSet = "CAP_SETUID CAP_SETGID CAP_DAC_READ_SEARCH";
            # Stop = SIGTERM to the runner only: it stops claiming and finishes the current job. A job that
            # hits its own timeout is interrupted then, and Terraform gets up to jobGrace to save its state
            # before it's killed, plus 5 minutes for a claim in flight and the report.
            KillMode = "mixed";
            TimeoutStopSec = "${rcfg.jobTimeout} ${rcfg.jobGrace} 5min";
          };
      };
    })
  ];
}
