---
title: Opsolete Docs
---

Opsolete is a self-hosted way to deploy your existing Terraform modules: pick a template, fill in a form, review the plan, apply. See [About Opsolete]({{< relref "/docs/about" >}}) for how it fits together.

## Get started

Run the app and at least one runner. Both are one binary, shipped as container images, a Helm chart and a NixOS module.

### Install

What every deployment needs, then a guide for each way to run it: [Install]({{< relref "/docs/install" >}}).

- [Docker]({{< relref "/docs/install/docker" >}}): try Opsolete on one machine in a few commands.
- [Kubernetes (Helm)]({{< relref "/docs/install/kubernetes" >}})
- [NixOS]({{< relref "/docs/install/nixos" >}})
- [Run from source]({{< relref "/docs/install/build" >}}), with or without Nix, with examples.

---

## Featured documentation

{{< cards >}}
  {{< card link="features/templates" title="Templates" subtitle="Turn a pinned Terraform module into a form: inputs, validation, conditional fields and where templates live." >}}
  {{< card link="features/runs" title="Runs" subtitle="Plan, review, apply: the kinds of runs, drift detection, logs and retention." >}}
  {{< card link="config/reference" title="Option reference" subtitle="Every setting of the server and the runner, with its default and NixOS option." >}}
  {{< card link="endpoints" title="Endpoint reference" subtitle="Every route the server registers, and who may call it." >}}
{{< /cards >}}
