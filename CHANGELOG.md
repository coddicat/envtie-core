# Changelog

All notable changes to EnvTie Core are documented here.

## Unreleased

## [1.8.0-beta.1] - 2026-10-29

- `vercel@1`: add Blob stores via `product: blob` (default remains Hosting). Creates one
  store per environment (`access`, optional `region` / `team_id`); outputs
  `BLOB_STORE_ID` and `BLOB_READ_WRITE_TOKEN` from the storage secrets API. Existing
  Hosting blueprints without `product` keep working.
- `upstash@1`: add Vector indexes via `product: vector` (default remains Redis). Vector
  config uses `region`, `similarity_function`, `dimension_count`, and optional
  `index_type` / embedding models; outputs `UPSTASH_VECTOR_REST_URL` and
  `UPSTASH_VECTOR_REST_TOKEN`. Existing Redis blueprints without `product` keep working.
- Official `resend@1` adapter: create/delete one domain-scoped Resend sending
  API key per environment (`sending_access` + `domain_id`). Outputs
  `RESEND_API_KEY` (create-only), `RESEND_API_KEY_ID`, and optional `RESEND_FROM`.
  Credential is a management `RESEND_API_KEY` with permission to manage keys.
- `mailtrap@1`: create via `POST /api/projects/{id}/sandboxes` with
  `{"sandbox":{"name":"..."}}` (current Mailtrap Email Sandbox API). Plan or
  permission denials map to `mailtrap_quota_exceeded` instead of a vague
  `mailtrap_invalid_request` / `mailtrap_unauthorized`.
- `mailtrap@1`: list inboxes via `GET /api/accounts/{id}/inboxes` (and filter by
  project) instead of `GET .../projects/{id}/inboxes`, which is create-only and
  returned 404 → `mailtrap_not_found` on read-before-create.
- `vercel@1`: omit Deployments API `target` for preview deploys. Sending
  `"target":"preview"` is invalid (only `production` / `staging` / custom env
  ids are accepted) and Vercel responds HTTP 400 → `vercel_invalid_request`.
- `vercel@1`: when `gitSource.ref` is a commit SHA, accept that SHA (or
  `githubCommitSha`) during validate/read so apply does not fail with
  `vercel_deployment_unmanaged`. Also send `gitMetadata.commitRef` for the
  EnvTie branch.
- `vercel@1`: deploy with the git **branch** as `gitSource.ref` (not the commit
  SHA) so Vercel injects gitBranch-scoped preview env vars. SHA-only refs skip
  those vars and the preview app shows providers as “Not configured”.
- Official `vercel@1` adapter: adopt an existing Vercel project (never create/delete the project). Each EnvTie environment maps to a git branch — upsert branch-scoped preview env vars from `vercel.env.*` bindings, trigger a git-sourced preview deployment, poll until ready, and output `VERCEL_URL`. Destroy removes that branch's env vars and deployments only. Does not use Vercel Custom Environments. Credential `VERCEL_TOKEN`; config `project_id` (+ optional `team_id`, `git_branch`).
- Official `mailtrap@1` adapter: create/delete one Mailtrap Email Sandbox inbox per environment, return SMTP outputs (`MAILTRAP_SMTP_*`, `MAILTRAP_INBOX_ID`), credential `MAILTRAP_API_TOKEN`, config `account_id` + `project_id`. Hosted control planes need this tag published (binaries + `adapters.json`), then a `go.mod` pin bump and `RUNNER_VERSION` redeploy before consumers can plan `mailtrap@1`.
- Official `sentry@1` adapter: create/delete one Sentry project per environment, return `SENTRY_DSN` and `SENTRY_PROJECT_SLUG`, credential `SENTRY_AUTH_TOKEN`. Hosted control planes need this tag published (binaries + `adapters.json`), then a `go.mod` pin bump and `RUNNER_VERSION` redeploy before consumers can plan `sentry@1`.
- Publish the runner image before attaching its provenance and SBOM, so a new GHCR package is not rejected with `read_package`.
- Added typed adapter runtime inputs and a non-mutating credential preflight. Local runners prompt once for missing values; noninteractive runners leave the approved operation queued.
- Prepared the public CLI, Action, packages, and runner image for `v1.5.0-beta.1`.
- Removed the hosted `preview execute --dispatch` and `preview report` commands.
  Pull-request and custom CI workflows use `preview apply`, `preview close`,
  `preview destroy`, `preview sweep`, and `preview status`.
- Extracted the customer-side CLI, runner, adapters, SDK, blueprint contracts,
  templates, GitHub Action, and packaging into the public `envtie-core`
  repository.
- Added independent PR, security, Docker smoke, and artifact release workflows.
