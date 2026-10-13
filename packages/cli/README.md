# EnvTie.dev CLI

The customer-side EnvTie.dev runner. The npm package selects a native binary for
the current operating system and architecture. Official adapters are downloaded
only when a signed plan needs them and are verified before execution.

Pin the package version in production.

GitHub App previews install this package from the workflow the dashboard
creates. That job runs:

```sh
envtie preview execute --dispatch <id> --timeout 15m
```

It reads `ENVTIE_API_URL` from the environment. Do not pass the API URL or any
secret as a command-line argument.

A self-hosted runner uses the same package:

```sh
export ENVTIE_API_URL=https://api.envtie.example.com
export ENVTIE_RUNNER_TOKEN=...
export ENVTIE_PUBLIC_KEY=...
npx --yes @envtie.dev/cli@1.0.0 runner execute
```

`runner execute` keeps polling. `runner execute --once` runs the jobs that are
waiting and exits. Custom CI systems use `preview apply`, `preview destroy`,
and `preview status`; those commands need `ENVTIE_CI_TOKEN` as well as
the runner token. GitHub App previews use the project API key and a short-lived GitHub App installation token.
