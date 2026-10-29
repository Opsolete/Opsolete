---
title: Template builder
weight: 2
---

The builder is on with a Git catalog in pull request or commit mode (see [Templates]({{< relref "templates#where-templates-live" >}})), or with `TEMPLATE_EDITOR=1` for `TEMPLATES_DIR`. Only admins can use it, because a template decides which module the runners execute.

1. Open **+ New template** (`/templates/new`) or **edit** on any template.
2. Choose **Start from a registry module** and the module source, version and one form field per variable are filled in for you. Complex variable types (list, map, object) are flagged, not generated.
3. Or type a `git::` source and ref by hand, then add one row per form field (variable name, label, type, required, pattern, dropdown options, default).

Each new template gets a random UUID as its id. Saving runs the same validation as loading, checks that a referenced registry version exists, and then opens a pull request or commits `<folder>/<id>.json` (Git catalog), or writes it into `TEMPLATES_DIR` and shows the generated deploy form right away. An edited template keeps its file name.
