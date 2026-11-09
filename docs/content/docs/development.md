---
title: Development
weight: 5
---

Set up a local environment first: see [Getting started]({{< relref "getting-started" >}}).

## Tests

```sh
just test                                    # in the nix dev shell: sqlc diff, gofmt, vet, all tests incl. DB tests
just test-linux                              # the non-DB tests on Linux in docker, plain and with --init
TEST_DATABASE_URL=postgres://… go test ./... # each DB test creates and drops its own database there (needs CREATEDB)
```

Some tests keep the docs honest: `internal/config` fails when an env var it reads is missing from [Configuration]({{< relref "/docs/operations/configuration" >}}), and `internal/http` when a route is missing from (or extra in) [Endpoints]({{< relref "/docs/operations/endpoints" >}}). `TestAccessMatrix` needs a row for every route.

```sh
go test -run 'TestDocumented|TestEndpointsDocumented' ./internal/config ./internal/http
```

## Documentation

This site lives in `docs/` (Hugo with the [Hextra](https://imfing.github.io/hextra/) theme, loaded as a Hugo module). Preview it with `just docs` and open http://localhost:1313/Opsolete/. Pages are in `docs/content/docs/`; link between them with `{{</* relref "…" */>}}`, so a broken link fails the build.

## CI and releases

`.github/workflows/ci.yml` runs on every PR and every push to `master`. Its jobs run in parallel:

- **`commits`** (PRs only): every commit in the PR must be a [Conventional Commit](https://www.conventionalcommits.org) (merge commits are skipped). PRs are merged with merge commits, so release-please reads each commit, not the PR title.
- **`test`:** gofmt, vet, all tests **with the race detector** against a Postgres 17 service, with the Terraform version from `.terraform-version` for the runner tests, and `govulncheck`.
- **`runner-linux`:** `just test-linux`: the tests in a Linux container, once with the test binary as PID 1 and once under tini (`docker --init`), the two ways the runner image runs. It proves the runner kills what a job leaves behind.
- **`nix`:** `sqlc diff` (generated code current), then the package and both images in **one** `nix build` (which also catches a stale `vendorHash`), then a Trivy scan of both images. Images are pushed **only when a release is created**.
- **`nixos-test`:** the NixOS module booted in a VM (`nix/test.nix`), with KVM.

Go comes prebuilt from cache.nixos.org (nixpkgs' `go`), so no job compiles a toolchain: keep `flake.lock` fresh (`nix flake update`) to get Go's security releases. Terraform's version is in `.terraform-version`, read by CI, `just test-linux` and the flake (update the flake's checksums with it).

`codeql.yml` runs CodeQL on PRs, pushes to `master` and weekly; findings land under *Security › Code scanning*. `docs.yml` builds this site on PRs that touch `docs/` and publishes it to GitHub Pages (https://opsolete.github.io/Opsolete/) on pushes to `master` that touch `docs/`. Dependabot proposes Go module and action updates weekly (the docs theme monthly), a week after their release; on its Go PRs, `vendorhash.yml` fixes `vendorHash` and pushes to the PR branch.

### Versions come from commit messages

[Conventional Commits](https://www.conventionalcommits.org) → SemVer, via [release-please](https://github.com/googleapis/release-please). Every commit counts, so write them as you'd want them in the changelog:

| Commit | Next version from 0.6.x | Once ≥ 1.0.0 |
|---|---|---|
| `fix: …`, `perf: …` | patch (0.6.x+1) | patch |
| `feat: …` | 0.7.0 | minor |
| `feat!: …` or a `BREAKING CHANGE:` footer | 0.7.0 (pre-1.0, breaking bumps minor) | major |
| `docs:`, `chore:`, `ci:`, `test:`, `refactor:`, … | no release | no release |

On each push to `master`, once every check passed, the `release-please` job at the end of `ci.yml` runs release-please, which keeps a **release PR** open. It holds the next version and the [`CHANGELOG.md`](https://github.com/Opsolete/Opsolete/blob/master/CHANGELOG.md) entries. Merging that PR tags `vX.Y.Z`, creates the GitHub release, and pushes `ghcr.io/opsolete/opsolete-app` and `-runner` (linux/amd64) tagged `X.Y.Z`, `X.Y`, `latest` and the commit's `sha-…`.

**Beta:** until 1.0.0, breaking changes only bump the minor version, and GitHub releases are marked *pre-release*. To go stable, remove `"prerelease": true` from `release-please-config.json` in a commit with a `Release-As: 1.0.0` footer.

### Locking down `master`

Repo settings, one-time:

1. **General → Pull Requests:** allow only *merge commits*, so every commit (and its `BREAKING CHANGE` footer) reaches release-please:
   ```sh
   gh api -X PATCH repos/Opsolete/Opsolete -F allow_merge_commit=true -F allow_squash_merge=false \
     -F allow_rebase_merge=false -F delete_branch_on_merge=true
   ```
2. **Rules → Rulesets → New branch ruleset** for `master`: require a pull request, require the status checks `commits`, `test`, `runner-linux`, `nix` and `nixos-test`, block force pushes, and restrict deletions. Don't require the docs check: it only runs when `docs/` changes.
3. **Release PR checks:** a release PR opened with the default `GITHUB_TOKEN` gets no CI run (GitHub shows *action required*), so its required checks never pass. Add a fine-grained PAT, or a GitHub App token with *contents* and *pull requests* write access, as the secret `RELEASE_PLEASE_TOKEN`.
4. **Images:** GHCR packages start out private. Make them public, or give your cluster a pull secret.
5. **Documentation:** **Settings → Pages → Source: GitHub Actions**, or:
   ```sh
   gh api -X POST repos/Opsolete/Opsolete/pages -f build_type=workflow
   ```
6. **Dependabot vendorHash fix:** a fine-grained PAT with *contents* write access on this repo, saved as `DEPENDABOT_PUSH_TOKEN` under **both** *Secrets → Dependabot* and *Secrets → Actions*. Pushes made with `GITHUB_TOKEN` wouldn't trigger CI on the fixed commit.
