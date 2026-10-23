# Example: Mailtrap Email Sandbox (custom adapter)

Mailtrap is **not** an official EnvTie catalog adapter. This folder is a sample private adapter you can build, pin, and run through the product **Custom adapter** path—the same path any user-built adapter uses.

To scaffold your own adapter from scratch (not Mailtrap), run `envtie adapter init <name>` — the CLI builds and prints the SHA-256 pin when Go is installed.

Each environment gets one Mailtrap [Email Sandbox](https://mailtrap.io) testing inbox. SMTP credentials are bound for the app; the API token stays on the runner.

## What you need

1. Mailtrap account + API token (Email Sandbox permissions)
2. Numeric **account id** and **project id** from the Mailtrap UI (URL or API)
3. A runner that can see the built binary and `MAILTRAP_API_TOKEN`

## Build and pin

```powershell
cd envtie-core
go build -trimpath -o dist/adapter-mailtrap.exe ./examples/custom-adapter-mailtrap
go run ./cmd/envtie adapter test ./dist/adapter-mailtrap.exe
```

On macOS/Linux use `dist/adapter-mailtrap` (no `.exe`). Copy the printed `sha256:` into the wizard or blueprint.

## Blueprint wizard (Custom adapter)

In the dashboard Blueprint flow, select **Custom adapter** (and optionally GitHub), then fill:

| Field | Example |
| --- | --- |
| Component id | `mail` |
| Adapter id | `mailtrap@1` |
| Executable path | `./dist/adapter-mailtrap` |
| SHA-256 pin | digest from `envtie adapter test` |
| Config | `account_id: 12345` and `project_id: 67890` (one per line; quotes optional) |
| Outputs to bind | `MAILTRAP_SMTP_HOST,MAILTRAP_SMTP_PORT,MAILTRAP_SMTP_USER,MAILTRAP_SMTP_PASS,MAILTRAP_INBOX_ID` |

## Blueprint (Write YAML)

```yaml
name: mailtrap-preview
components:
  - id: mail
    adapter: mailtrap@1
    executable: ./dist/adapter-mailtrap.exe   # path on the runner machine
    sha256: REPLACE_WITH_ADAPTER_TEST_SHA256
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

Notes:

- `name` is injected by the runner as `<environment>-mail` (do not put secrets in `config`)
- Prefer GitHub Environment bindings (`github.environment.secret.*`) when you also use the GitHub provider
- Windows runners need the `.exe` path; Unix runners must not use `.exe`

## Runner credentials

```powershell
$env:MAILTRAP_API_TOKEN = "<token>"
# plus ENVTIE_API_URL, ENVTIE_RUNNER_TOKEN, ENVTIE_PUBLIC_KEY as usual
npx --yes "@envtie.dev/cli@<version>" runner execute
```

## End-to-end checklist

1. Build + `envtie adapter test` → pin `sha256`
2. Save blueprint via **Custom adapter** in the wizard (or Write YAML)
3. Start a runner with `MAILTRAP_API_TOKEN` and the binary at the pinned path
4. Plan/approve an environment (e.g. `manual01`)
5. Confirm inbox in Mailtrap Email Sandbox and `.envtie/bindings/manual01.env` (or GitHub env secrets)
6. Destroy the environment → inbox is deleted

## Tests

```powershell
go test ./examples/custom-adapter-mailtrap/ -count=1
```

For a minimal private adapter without a third-party API, see `../custom-adapter`.
