package main

import (
	"github.com/coddicat/envtie-core/adapters/stripe"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func main() { adapter.Serve(stripe.Handler{}) }
