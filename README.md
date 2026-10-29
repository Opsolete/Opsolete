# Opsolete
From forms to infrastructure. Automatically.

Pick a template, name a **workspace** and fill in a form generated from JSON. A **Opsolete runner** plans the pinned Terraform module, you review the plan and confirm it, and a runner applies exactly that plan. You watch the output live in the browser, and the Terraform outputs show up when it's done.

**Documentation: https://opsolete.github.io/Opsolete/** (source in [`docs/`](docs/content/docs))

## Quick start

```sh
nix develop        # or `direnv allow`
just run           # postgres + app on :8080
just runner        # in a 2nd terminal: a runner on this machine
just reseed        # load the examples
just docs          # preview the documentation
```

Without Nix, configuration, deployment, CI and releases: see the [documentation](https://opsolete.github.io/Opsolete/). Changes are in [CHANGELOG.md](CHANGELOG.md).
