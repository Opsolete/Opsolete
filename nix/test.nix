# Server with a local database and secrets from files, a runner registered with a token from
# opsolete-admin. Run: nix build .#checks.x86_64-linux.nixos -L
self: {
  name = "opsolete";
  nodes.machine = {pkgs, ...}: {
    imports = [self.nixosModules.default];
    virtualisation.memorySize = 2048;
    # stand-ins for sops-nix secrets: root-owned files outside the store
    systemd.tmpfiles.rules = ["f+ /run/opsolete-secret-key 0400 root root - 0123456789abcdef0123456789abcdef"];
    services.opsolete = {
      enable = true;
      publicUrl = "http://opsolete.test";
      secretKeyFile = "/run/opsolete-secret-key";
      devUser = "admin@test";
      adminEmails = ["admin@test"];
      runner = {
        enable = true;
        tokenFile = "/run/opsolete-runner-token";
        terraformPackage = pkgs.opentofu; # terraform is unfree
      };
    };
  };
  testScript = ''
    machine.wait_for_unit("opsolete.service")
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:8080/readyz")
    # the secret must not leak into the unit's environment
    machine.fail("systemctl show opsolete -p Environment | grep 0123456789abcdef")
    machine.succeed("opsolete-admin token runner-1 runner > /run/opsolete-runner-token")
    machine.succeed("systemctl restart opsolete-runner")
    machine.wait_until_succeeds("journalctl -u opsolete-runner | grep 'runner started'")
    # PrivatePIDs: the runner is PID 1 of its namespace, so it doesn't warn about job isolation
    machine.fail("journalctl -u opsolete-runner | grep 'not running as a container'")
    machine.sleep(5)  # a rejected token would have stopped it by now
    machine.succeed("systemctl is-active opsolete-runner")
  '';
}
