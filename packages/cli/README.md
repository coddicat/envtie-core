# EnvTie.dev CLI

The customer-side EnvTie.dev runner. The npm package selects a native binary for
the current operating system and architecture. Official adapters are downloaded
only when a signed plan needs them and are verified before execution.

Pin the package version in production.

GitHub App pull-request previews install this package from the workflow the
dashboard creates. That job uses the project API key and a short-lived GitHub
App installation token:

```sh
envtie preview apply pr-15 --run
envtie preview close pr-15 --run
envtie preview sweep --run
```

`preview apply`, `preview close`, `preview destroy`, and `preview sweep` accept
`--run` to lease and finish that operation in the same process. `preview status`
reads one operation. These commands call the custom CI API. They need
`ENVTIE_API_URL` and either `ENVTIE_API_KEY` or `ENVTIE_CI_TOKEN`. Do not pass
the API URL or any secret as a command-line argument.

`envtie github setup` prints the GitHub App manifest. GitHub issues the private
key to you; this package does not store it.

A self-hosted runner uses the same package:

```sh
export ENVTIE_API_URL=https://api.envtie.example.com
export ENVTIE_RUNNER_TOKEN=...
export ENVTIE_PUBLIC_KEY=...
npx --yes @envtie.dev/cli@1.0.0 runner execute
```

`runner execute` keeps polling. `runner execute --once` runs the jobs that are
waiting and exits. Workspace runners lease, checkpoint, and report on
`/v1/runner/jobs`. A preview command with `--run` does the same on
`/v1/custom-ci/operations`.
