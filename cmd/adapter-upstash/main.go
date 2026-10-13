package main

import (
	"github.com/coddicat/envtie-core/adapters/upstash"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func main() { adapter.Serve(upstash.Handler{}) }
