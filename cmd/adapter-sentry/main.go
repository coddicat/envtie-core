package main

import (
	"github.com/coddicat/envtie-core/adapters/sentry"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func main() { adapter.Serve(sentry.Handler{}) }
