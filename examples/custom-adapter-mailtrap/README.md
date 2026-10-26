# Mailtrap Email Sandbox (official `mailtrap@1`)

Mailtrap is an **official** EnvTie catalog adapter. Use **Mailtrap** in the
dashboard Blueprint wizard (or Write YAML with `adapter: mailtrap@1`). Do not
pin a local binary.

This folder kept a thin `main` for historical links; the implementation lives in
`adapters/mailtrap`.

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
