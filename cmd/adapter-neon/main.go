package main

import (
	"github.com/coddicat/envtie-core/adapters/neon"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func main() { adapter.Serve(neon.Handler{}) }
