# Contributing

Open an issue before a large protocol or compatibility change. Keep customer credentials and private repository code
out of this public repository.

Before submitting:

```sh
gofmt -w .
go vet ./...
go test -race ./...
node --check packages/cli/bin/envtie.js
```

Update tests, `CHANGELOG.md`, schemas, templates, and documentation together. Adapter errors must use stable sanitized
codes and must not include provider response bodies. Commits must not modify an existing release tag or published
artifact.

By contributing, you agree that your contribution is licensed under Apache-2.0.
