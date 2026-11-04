---
title: Template builder
weight: 2
---

The builder is on with a Git catalog in pull request or commit mode (see [Templates]({{< relref "templates#where-templates-live" >}})), or with `TEMPLATE_EDITOR=1` for `TEMPLATES_DIR`. Only admins can use it, because a template decides which module the runners execute.

1. Open **+ New template** (`/templates/new`) or **edit** on any template.
2. Choose **Start from a registry module** and the module source, version and one form field per variable are filled in for you. Complex variable types (list, map, object) are flagged, not generated. A string variable's `validation` written as `can(regex("<literal>", var.<name>))` fills its pattern; other conditions stay with Terraform, which checks them at plan time. Patterns use Go (RE2) syntax, but the browser checks them as JavaScript regular expressions: Go-only syntax such as `(?i)` makes the browser skip its check, and the server still rejects values that don't match.
3. Or type a `git::` source and ref by hand, then add one card per form field: variable name, label, type (Text, Number, Yes / no, Dropdown), and the settings that type uses: dropdown options as `value` or `Label=value`, default, pattern, required, sensitive. Hover a setting's name for help. Reorder fields with ↑ ↓ and remove them with ✕ (or clear the variable name).
4. To ask only what matters, set a field's **Show** row to *when &lt;dropdown&gt; is* and pick the values (click the chips, or type them comma-separated). Only dropdowns above the field can be picked. See [conditional inputs]({{< relref "templates#conditional-inputs" >}}).

Each new template gets a random UUID as its id. Saving runs the same validation as loading, checks that a referenced registry version exists, and then opens a pull request or commits `<folder>/<id>.json` (Git catalog), or writes it into `TEMPLATES_DIR` and shows the generated deploy form right away. An edited template keeps its file name.
