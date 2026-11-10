---
title: Opsolete
layout: hextra-home
---

{{< hextra/hero-headline >}}
Pick. Plan. Apply.
{{< /hextra/hero-headline >}}

<div class="hx:mb-6">
{{< hextra/hero-subtitle >}}
A self-hosted, no-code way to deploy your existing Terraform modules:&nbsp;<br class="hx:sm:block hx:hidden" />pick a template, fill in a form, review the plan, apply.
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx:mb-12">
{{< hextra/hero-button text="Get started" link="docs" >}}
</div>

{{< hextra/feature-grid >}}
  {{< hextra/feature-card title="Plan, then apply" subtitle="Every change is a run: a plan you review and confirm, then exactly that plan applied." link="docs/features/runs" >}}
  {{< hextra/feature-card title="Forms from JSON" subtitle="Templates pin an approved Terraform module and generate the form for its inputs." link="docs/features/templates" >}}
  {{< hextra/feature-card title="Private module registry" subtitle="Upload modules or publish them from GitHub and Azure DevOps tags." link="docs/features/module-registry" >}}
  {{< hextra/feature-card title="State backend" subtitle="Encrypted, versioned state with locking and rollback, on any blob store." link="docs/features/state" >}}
  {{< hextra/feature-card title="Your own runners" subtitle="Runners pull jobs over HTTPS: no database access, no inbound connections." link="docs/config/runners" >}}
  {{< hextra/feature-card title="Teams from your IdP" subtitle="OIDC login, per-workspace access from IdP groups and a tamper-evident audit log." link="docs/config/authentication" >}}
{{< /hextra/feature-grid >}}
