# Minimal custom adapter

Prefer the CLI (no manual `go get` / `go build`):

```powershell
npx --yes "@envtie.dev/cli@<version>" adapter init myprovider
cd adapter-myprovider
# init already built dist/ and printed sha256:
# pin adapter myprovider@1 in the dashboard Custom adapter step
```

After editing code:

```powershell
npx --yes "@envtie.dev/cli@<version>" adapter build
```

Requires [Go](https://go.dev/dl/). Pass `-no-build` on init if you only want the files.

This folder is a hand-written file-backed sample (adapter name `example`). Official catalog adapters stay in the wizard by name; everything else uses the Custom adapter step.

## Runner credentials (this example)

| Variable | Purpose |
| --- | --- |
| `ENVTIE_EXAMPLE_STATE_DIR` | Directory for per-environment state files |
| `ENVTIE_EXAMPLE_SECRET` | Value returned as `EXAMPLE_VALUE` |

For a real third-party provider sample (Mailtrap Email Sandbox), see `../custom-adapter-mailtrap`.
