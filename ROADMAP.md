# Roadmap

## Under consideration

### Dynamic cloud credentials (OIDC workload identity federation)

**Status:** undecided. Azure Entra ID has no wildcard subject matching for custom issuers (see below).

**Goal:** remove long-lived cloud secrets (`ARM_CLIENT_SECRET` and similar) from the database. Each run gets a short-lived, signed token instead.

**How it would work**

- Opsolete acts as an OIDC issuer. It signs a short-lived JWT per run (TTL about 10 minutes) with claims such as `workspace`, `template`, `repo`, `run_id` and `run_phase`.
- Runs exchange the token for cloud credentials:
  - **Azure:** `ARM_USE_OIDC=true` plus `ARM_OIDC_TOKEN_FILE_PATH`
  - **AWS:** `AWS_ROLE_ARN` plus `AWS_WEB_IDENTITY_TOKEN_FILE`
  - **GCP:** an `external_account` credential file
- New pieces: a signing key (kept in KMS or Key Vault and never stored in the database), a JWKS endpoint, an `/.well-known/openid-configuration` document and key rotation via `kid`.

**Development and hosting**

- Only the discovery document and the JWKS must be public over HTTPS, and both are static files (an S3 bucket, a Blob static website and so on). Signing can happen anywhere, including `localhost`.
- GCP can take an uploaded JWKS, so it needs no public URL. AWS and Azure need one.
- Use one issuer per Opsolete environment (dev, prod), each with its own key, so a dev key can never mint tokens that prod accepts.
- Keep static secrets as a credential mode (`static` or `oidc` per connection) for local development and tests.

**Blocker: Entra ID only does exact matching**

Entra federated identity credentials match `iss`, `sub` and `aud` exactly, with a limit of 20 per identity. [Flexible FIC](https://learn.microsoft.com/en-us/entra/workload-id/workload-identities-flexible-federated-identity-credentials) (wildcards) is in preview and only supports GitHub, GitLab and HCP Terraform as issuers, so a rule like `workspace:prod-*` isn't possible. AWS (`StringLike`) and GCP (CEL conditions) do support wildcards.

**Options for Azure if this goes ahead**

1. **One identity per environment and phase (preferred).** For example, `ops-prod-plan` with `sub = env:prod:phase:plan` (Reader) and `ops-prod-apply` with `sub = env:prod:phase:apply` (Contributor). Opsolete maps workspace → environment and decides which `sub` to mint.
2. **One user-assigned managed identity per workspace,** created automatically when a workspace is created, with an exact FIC. This gives blast-radius isolation, but Opsolete then needs rights to create identities and role assignments.
3. **Wait** until flexible FIC supports custom issuers.

**Risks**

- With exact matching, Opsolete becomes the policy decision point, so its subject-minting code is security-critical. It needs negative tests: a dev workspace must never get an `env:prod` subject, and a plan run must never get `phase:apply`.
- Cloud access tokens last about an hour, so check how each provider re-exchanges tokens during long applies.
