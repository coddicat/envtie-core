// Historical sample entrypoint. Prefer the official mailtrap@1 catalog adapter
// (adapters/mailtrap, cmd/adapter-mailtrap) via the Blueprint wizard—no local
// executable or sha256 pin required.
package main

import (
	"github.com/coddicat/envtie-core/adapters/mailtrap"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func main() { adapter.Serve(mailtrap.Handler{}) }
