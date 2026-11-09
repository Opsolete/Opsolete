# Opsolete

**Fill in a form. Get real infrastructure.**

Most teams have a few people who write good Terraform and a lot of people who just need a storage account, a resource group or a VM by Friday. Opsolete sits between them. The people who know Terraform turn their modules into templates. Everyone else picks a template, fills in a short form, looks at the plan and clicks apply.

It's self-hosted, it runs your existing modules as they are, and it doesn't hide what Terraform is doing: every change is a run you can follow line by line in the browser.

## What's in the box

- **Forms from a few lines of JSON.** A template points at a pinned module version and describes its inputs. Opsolete builds the form, checks the values and keeps sensitive ones encrypted.
- **Plan first, then apply.** Nothing changes until someone has read the plan and confirmed it, and then exactly that plan is applied. Auto-apply is there if you want it.
- **Your own runners.** Runners pull work over HTTPS, so they need no database access and no open ports, and they clean up after every job.
- **State you don't have to think about.** Opsolete is the Terraform backend: encrypted, versioned and locked, with rollback, on S3, Azure Blob Storage, Google Cloud Storage or a local disk.
- **Fits the tools you already use.** A private module registry, GitHub and Azure DevOps (deploy on push, plans on pull requests), and the Terraform CLI through the `cloud` block for people who'd rather stay in their terminal.
- **Teams from your identity provider.** OIDC login, access per project or workspace by IdP group, and an audit log that notices when someone tampers with it.

## Try it

With [Nix](https://nixos.org/download/) it's a few commands. Without it you need Go, PostgreSQL and Terraform; the [documentation](https://opsolete.github.io/Opsolete/) walks you through that.

```sh
nix develop        # or `direnv allow`
just reseed        # wipe the dev database and load the examples (stops postgres: run it first)
just run           # postgres + app on :8080
just runner        # in a second terminal: a runner on this machine
```

Then open http://opsolete.localhost:8080. Locally you're signed in as `dev@localhost`, an admin, so you can try everything straight away.

Working on Opsolete itself? `just check` runs the tests and a smoke test, and `just docs` previews the documentation.

## Where it's at

Opsolete is at 0.6 and heading for 1.0. Version 0.6 was a rebuild and needs a fresh install: see [Upgrading to 0.6](https://opsolete.github.io/Opsolete/docs/operations/upgrading/). What changed in each release is in [CHANGELOG.md](CHANGELOG.md).

## Documentation

Everything else, from writing templates to running Opsolete on Kubernetes, is at **https://opsolete.github.io/Opsolete/**. The source is in [`docs/`](docs/content/docs).
