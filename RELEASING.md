# Releasing EnvTie Core

For `v1.5.0-beta.1`, configure npm trusted publishers for `@envtie.dev/cli` and all six platform packages, and grant
this repository Actions write access to `ghcr.io/coddicat/envtie-runner`. The Release workflow is fail-closed: npm,
GHCR, and binaries must all succeed before the GitHub Release is created.

1. Require the `Core · required` check on `main`.
2. Confirm `Docker smoke` and `Security` are green for the release commit.
3. Update `CHANGELOG.md`.
4. Run **Tag release** on `main` with a new semantic version, for example
   `v1.4.0-rc.3`. It refuses an existing tag and requires CI and Security to be
   green for that commit. The latest Docker smoke run on `main` must also be
   green. The workflow pushes the annotated tag and starts **Release**.
5. Watch the Release workflow publish binaries, `adapters.json`, npm packages,
   attestations, and `ghcr.io/coddicat/envtie-runner`.
6. Verify `SHA256SUMS.txt` and download one platform package.
7. Set `ENVTIE_RUNNER_VERSION` in the private EnvTie.dev repository to this tag.

Creating a core release never deploys EnvTie.dev. Production deployment remains
a manual action in the private repository.

## Compatibility

The private control plane signs plans containing adapter URLs and SHA-256
digests from one pinned `adapters.json`. A runner executes an adapter only after
its digest matches the signed plan. Keep release filenames stable:

`adapter-<provider>_<tag>_<os>_<arch>[.exe]`

Rollback the hosted product by selecting the previous immutable private image
tag. Roll back runner distribution by pinning a prior core version; never
overwrite an existing tag.
