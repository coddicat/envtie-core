package main

import (
	"context"
	"fmt"
	"os"

	"github.com/coddicat/envtie-core/sdk/adapter"
)

type handler struct{}

func (handler) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Name:         "neon",
		Version:      "1.0.0",
		Protocol:     adapter.ProtocolVersion,
		Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"},
		Outputs:      []adapter.Field{{Name: "DATABASE_URL", Sensitivity: "SECRET"}},
	}
}

func (handler) Handle(_ context.Context, r adapter.Request) (adapter.Response, error) {
	const leak = "postgresql://user:secret@db.example/app https://console.neon.tech/api BODY-MARKER"
	fmt.Fprintln(os.Stderr, leak)
	switch r.Config["mode"] {
	case "wrapped":
		return adapter.Response{}, fmt.Errorf("%s: %w", leak, adapter.NewError("neon_quota"))
	case "bad":
		return adapter.Response{}, adapter.NewError("https://user:secret@example.com/BODY-MARKER")
	default:
		return adapter.Response{}, adapter.NewError("neon_quota")
	}
}

func main() {
	adapter.Serve(handler{})
}
