package main

import (
	"github.com/coddicat/envtie-core/adapters/mailtrap"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func main() { adapter.Serve(mailtrap.Handler{}) }
