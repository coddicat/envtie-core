# Minimal custom adapter

Private adapters are ordinary binaries. Prefer scaffolding a new one:

```powershell
envtie adapter init myprovider
cd adapter-myprovider
go mod tidy
go build -trimpath -o dist/adapter-myprovider.exe .
envtie adapter test ./dist/adapter-myprovider.exe
```

Then choose **Custom adapter** in the EnvTie.dev Blueprint wizard and pin the executable + SHA-256.

This folder is a hand-written file-backed sample (adapter name `example`). Prefer `envtie adapter init` for new adapters. Official catalog adapters stay in the wizard by name; everything else uses the Custom adapter step.

## Runner credentials (this example)

| Variable | Purpose |
| --- | --- |
| `ENVTIE_EXAMPLE_STATE_DIR` | Directory for per-environment state files |
| `ENVTIE_EXAMPLE_SECRET` | Value returned as `EXAMPLE_VALUE` |

For a real third-party provider sample (Mailtrap Email Sandbox), see `../custom-adapter-mailtrap`.
