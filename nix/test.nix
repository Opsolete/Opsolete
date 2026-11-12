# Server with a local database and secrets from files, and a runner whose token the server registers at start. Run: nix build .#checks.x86_64-linux.nixos -L
self: {
  name = "opsolete";
  nodes.machine = {pkgs, ...}: {
    imports = [self.nixosModules.default];
    virtualisation.memorySize = 2048;
    # stand-ins for sops-nix secrets: root-owned files outside the store
    systemd.tmpfiles.rules = [
      "f+ /run/opsolete-secret-key 0400 root root - 0123456789abcdef0123456789abcdef"
      "f+ /run/opsolete-runner-token 0400 root root - fedcba9876543210fedcba9876543210"
    ];
    services.opsolete = {
      enable = true;
      publicUrl = "http://opsolete.test";
      secretKeyFile = "/run/opsolete-secret-key";
      runnerTokenFile = "/run/opsolete-runner-token"; # registered as runner "machine"; the runner's tokenFile defaults to it
      devUser = "admin@test";
      adminEmails = ["admin@test"];
      runner = {
        enable = true;
        terraformPackage = pkgs.opentofu; # terraform is unfree
      };
    };
  };
  testScript = ''
    machine.wait_for_unit("opsolete.service")
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:8080/readyz")
    # the secret must not leak into the unit's environment
    machine.fail("systemctl show opsolete -p Environment | grep 0123456789abcdef")
    # no token step: the server registered the runner's token before it started
    machine.fail("systemctl show opsolete -p Environment | grep fedcba9876543210")
    machine.wait_until_succeeds("journalctl -u opsolete-runner | grep 'runner started'")
    # the job sandbox works under the unit's hardening (OPS_SANDBOX=namespaces: else the runner doesn't start)
    machine.succeed("journalctl -u opsolete-runner | grep 'job sandbox: namespaces'")
    machine.sleep(5)  # a rejected token would have stopped it by now
    machine.succeed("systemctl is-active opsolete-runner")
    # another runner's own token, for a host elsewhere
    machine.succeed("opsolete-admin runner-token -pool prd elsewhere | grep -q '^opsr_'")
    # it read the token as root and dropped to its user; jobs (same user) find no credentials directory
    machine.succeed("test $(stat -c %U /proc/$(systemctl show -p MainPID --value opsolete-runner)) = opsolete-runner")
    machine.fail("test -e /run/credentials/opsolete-runner.service")
    # a stop waits for the current job: its timeout (1h), OPS_JOB_GRACE to save state (10m) and 5 minutes to report
    machine.succeed("systemctl show -p TimeoutStopUSec --value opsolete-runner | grep -x '1h 15min'")
  '';
}
