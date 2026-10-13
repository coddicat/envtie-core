# Releasing EnvTie Core

1. Require the `Core · required` check on `main`.
2. Confirm `Docker smoke` and `Security` are green for the release commit.
3. Update `CHANGELOG.md`.
4. Create an annotated semantic-version tag and push it only after approval.
5. Watch the Release workflow publish binaries, `adapters.json`, npm packages,
   attestations, and `ghcr.io/coddicat/envtie-runner`.
6. Verify `SHA256SUMS.txt` and download one platform package.
7. Update the private EnvTie.dev repository's pinned core version separately.

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
