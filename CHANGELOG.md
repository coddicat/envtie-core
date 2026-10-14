# Changelog

All notable changes to EnvTie Core are documented here.

## Unreleased

- Removed the hosted `preview execute --dispatch` and `preview report` commands.
  Pull-request and custom CI workflows use `preview apply`, `preview close`,
  `preview destroy`, `preview sweep`, and `preview status`.
- Extracted the customer-side CLI, runner, adapters, SDK, blueprint contracts,
  templates, GitHub Action, and packaging into the public `envtie-core`
  repository.
- Added independent PR, security, Docker smoke, and artifact release workflows.
