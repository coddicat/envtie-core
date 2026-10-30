# Mailtrap Email Sandbox (official `mailtrap@1`)

Mailtrap is an **official** EnvTie catalog adapter. Use **Mailtrap** in the
dashboard Blueprint wizard (or Write YAML with `adapter: mailtrap@1`). Do not
pin a local binary.

This folder kept a thin `main` for historical links; the implementation lives in
`adapters/mailtrap`.

## Modes

### Dedicated (default)

One Email Sandbox per environment. Apply creates it; destroy deletes it. Use
this when hard isolation matters and your Mailtrap plan allows the sandbox
count.

### Shared

Set `mode: shared` plus `recipient_domain` (required). All environments in the
same Mailtrap project share **one** sandbox (`shared_sandbox_name`, or
`envtie-shared` when omitted). Apply creates that sandbox if it is absent;
destroy does **not** delete it, clear its messages, or adopt a sandbox whose
name does not match. Deleting the shared sandbox is a manual operator action.

Outputs still include the usual `MAILTRAP_SMTP_*` values (identical across
environments — expected) plus `MAILTRAP_RECIPIENT_TAG`, a deterministic address
`<sanitized-environment>@<recipient_domain>`. Tests must isolate by searching
or fetching messages filtered to that recipient via the Mailtrap API;
unfiltered assertions can see other environments' mail.

**Trade-offs:** SMTP credentials are shared across every environment using the
shared sandbox — rotation and leaks affect all of them. Messages from parallel
environments land in one inbox. Prefer dedicated mode when hard isolation
matters and the plan allows the sandbox count. Shared mode consumes exactly one
sandbox regardless of environment count (quota errors on first create still map
to `mailtrap_quota_exceeded`).

## Blueprint

```yaml
name: mailtrap-preview
components:
  - id: mail
    adapter: mailtrap@1
    ownership: managed
    config:
      account_id: "12345"
      project_id: "67890"
      # Optional shared mode (one sandbox for the project):
      # mode: shared
      # recipient_domain: mail.example.test
      # shared_sandbox_name: envtie-shared
bindings:
  - from: mail.MAILTRAP_SMTP_HOST
    to: dotenv.MAILTRAP_SMTP_HOST
  - from: mail.MAILTRAP_SMTP_PORT
    to: dotenv.MAILTRAP_SMTP_PORT
  - from: mail.MAILTRAP_SMTP_USER
    to: dotenv.MAILTRAP_SMTP_USER
  - from: mail.MAILTRAP_SMTP_PASS
    to: dotenv.MAILTRAP_SMTP_PASS
  - from: mail.MAILTRAP_INBOX_ID
    to: dotenv.MAILTRAP_INBOX_ID
  # When mode is shared, also bind:
  # - from: mail.MAILTRAP_RECIPIENT_TAG
  #   to: dotenv.MAILTRAP_RECIPIENT_TAG
policy:
  expires_in: 1d
  protect: false
```

## Runner

```powershell
$env:MAILTRAP_API_TOKEN = "<token>"
# plus ENVTIE_API_URL, ENVTIE_RUNNER_TOKEN, ENVTIE_PUBLIC_KEY
npx --yes "@envtie.dev/cli@<version>" runner execute
```

Account and project ids come from the Mailtrap UI or `GET /api/accounts` and
`GET /api/accounts/{id}/projects`.
