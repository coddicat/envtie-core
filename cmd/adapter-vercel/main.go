package main

import (
	"github.com/coddicat/envtie-core/adapters/vercel"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func main() { adapter.Serve(vercel.Handler{}) }
