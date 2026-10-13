package main

import (
	"github.com/coddicat/envtie-core/adapters/github"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func main() { adapter.Serve(github.Handler{}) }
