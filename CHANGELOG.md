# Changelog

All notable changes to EnvTie Core are documented here.

## Unreleased

- Added typed adapter runtime inputs and a non-mutating credential preflight. Local runners prompt once for missing values; noninteractive runners leave the approved operation queued.
- Prepared the public CLI, Action, packages, and runner image for `v1.5.0-beta.1`.
- Removed the hosted `preview execute --dispatch` and `preview report` commands.
  Pull-request and custom CI workflows use `preview apply`, `preview close`,
  `preview destroy`, `preview sweep`, and `preview status`.
- Extracted the customer-side CLI, runner, adapters, SDK, blueprint contracts,
  templates, GitHub Action, and packaging into the public `envtie-core`
  repository.
- Added independent PR, security, Docker smoke, and artifact release workflows.
