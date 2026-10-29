---
title: Development
weight: 5
---

Set up a local environment first: see [Getting started]({{< relref "getting-started" >}}).

## Tests

```sh
just test                                                     # in the nix dev shell: gofmt, vet, all tests incl. DB tests
go test ./...
TEST_DATABASE_URL=$DATABASE_URL go test ./internal/storage   # database tests
```

## Documentation

This site lives in `docs/` (Hugo with the [Hextra](https://imfing.github.io/hextra/) theme, loaded as a Hugo module). Preview it with `just docs` and open http://localhost:1313/Opsolete/. Pages are in `docs/content/docs/`; link between them with `{{</* relref "…" */>}}`, so a broken link fails the build.

## CI and releases

`.github/workflows/ci.yml` runs on every PR and every push to `master`:

- **`test`:** gofmt, vet, and all tests against a Postgres service, with Terraform 1.17.0 for the runner tests.
- **`nix`:** `nix build`, which also catches a stale `vendorHash`.
- **`images`:** builds the `app` and `runner` images. They're pushed **only when a release is created**.

`.github/workflows/docs.yml` builds this site on PRs that touch `docs/` and publishes it to GitHub Pages (https://opsolete.github.io/Opsolete/) on every push to `master` that touches `docs/`.

### Versions come from commit messages

[Conventional Commits](https://www.conventionalcommits.org) → SemVer, via [release-please](https://github.com/googleapis/release-please):

| Commit (= PR title, squash-merged) | Next version from 0.4.2 | Once ≥ 1.0.0 |
|---|---|---|
| `fix: …`, `perf: …` | 0.4.3 | patch |
| `feat: …` | 0.5.0 | minor |
| `feat!: …` or a `BREAKING CHANGE:` footer | 0.5.0 (pre-1.0, breaking bumps minor) | major |
| `docs:`, `chore:`, `ci:`, `test:`, `refactor:`, … | no release | no release |

After each merge, release-please keeps a **release PR** open. It holds the next version and the [`CHANGELOG.md`](https://github.com/Opsolete/Opsolete/blob/master/CHANGELOG.md) entries. Merging that PR tags `vX.Y.Z`, creates the GitHub release, and pushes `ghcr.io/opsolete/opsolete-app` and `-runner` tagged `X.Y.Z`, `X.Y` and `latest`.

**Beta:** the first release is 0.1.0 (`initial-version`). Until 1.0.0, breaking changes only bump the minor version, and GitHub releases are marked *pre-release*. To go stable, remove `"prerelease": true` from `release-please-config.json` in a commit with a `Release-As: 1.0.0` footer.

`pr-title.yml` rejects PRs whose title isn't a Conventional Commit. With squash merges the title becomes the commit on `master`, so history stays parseable.

### Locking down `master`

Repo settings, one-time:

1. **General → Pull Requests:** allow only *squash merging*, and set its default commit message to *Pull request title*:
   ```sh
   gh api -X PATCH repos/Opsolete/Opsolete -F allow_squash_merge=true -F allow_merge_commit=false \
     -F allow_rebase_merge=false -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=PR_BODY -F delete_branch_on_merge=true
   ```
2. **Rules → Rulesets → New branch ruleset** for `master`: require a pull request, require the status checks `test`, `nix`, `images (app)`, `images (runner)` and `conventional-commit`, block force pushes, and restrict deletions. Don't require the docs check: it only runs when `docs/` changes.
3. **Release PR checks:** PRs opened with the default `GITHUB_TOKEN` don't trigger workflows, so the release PR would never get its required checks. Add a fine-grained PAT, or a GitHub App token with *contents* and *pull requests* write access, as the secret `RELEASE_PLEASE_TOKEN`.
4. **Images:** GHCR packages start out private. Make them public, or give your cluster a pull secret.
5. **Documentation:** **Settings → Pages → Source: GitHub Actions**, or:
   ```sh
   gh api -X POST repos/Opsolete/Opsolete/pages -f build_type=workflow
   ```
