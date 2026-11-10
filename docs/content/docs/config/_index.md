---
title: Configuration
weight: 3
---

The server and the runner are configured with environment variables, the same with every way to [install]({{< relref "/docs/install" >}}) them. Everything else, such as projects, access, variables and VCS connections, is set in the web UI.

{{< cards >}}
  {{< card link="reference" title="Option reference" subtitle="Every environment variable of the server and the runner." >}}
  {{< card link="authentication" title="Login and access" subtitle="OIDC login, roles, team access per workspace and the audit log." >}}
  {{< card link="runners" title="Runners" subtitle="How runners claim and run jobs, their credentials and cost estimation." >}}
  {{< card link="secret-key" title="SECRET_KEY" subtitle="What it protects, how to back it up and how to rotate it." >}}
{{< /cards >}}
