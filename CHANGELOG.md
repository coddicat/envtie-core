# Changelog

All notable changes to EnvTie Core are documented here.

## Unreleased

- `vercel@1`: omit Deployments API `target` for preview deploys. Sending
  `"target":"preview"` is invalid (only `production` / `staging` / custom env
  ids are accepted) and Vercel responds HTTP 400 → `vercel_invalid_request`.
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
