package main

import (
	"github.com/coddicat/envtie-core/adapters/resend"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func main() { adapter.Serve(resend.Handler{}) }
