package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/coddicat/envtie-core/sdk/adapter"
)

type handler struct{}

func (handler) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Name:         "neon",
		Version:      "1.0.0",
		Protocol:     adapter.ProtocolVersion,
		Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"},
		CredentialEnv: []string{
			"NEON_API_KEY",
			"ENVTIE_E2E_STATE_DIR",
		},
		Outputs: []adapter.Field{{Name: "DATABASE_URL", Sensitivity: "SECRET"}},
	}
}

func (handler) Handle(_ context.Context, request adapter.Request) (adapter.Response, error) {
	dir := os.Getenv("ENVTIE_E2E_STATE_DIR")
	if dir == "" || os.Getenv("NEON_API_KEY") == "" {
		return adapter.Response{}, errors.New("fixture credentials missing")
	}
	id := request.Environment + "-" + request.Component
	if request.ExternalID != "" && request.ExternalID != id {
		return adapter.Response{}, errors.New("fixture identity mismatch")
	}
	file := filepath.Join(dir, id+".txt")
	switch request.Hook {
	case "read", "validate":
		if _, err := os.Stat(file); os.IsNotExist(err) {
			return adapter.Response{Absent: true}, nil
		} else if err != nil {
			return adapter.Response{}, err
		}
		return adapter.Response{ExternalID: id, Status: "ready"}, nil
	case "apply":
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return adapter.Response{}, err
		}
		if err := os.WriteFile(file, []byte("fixture neon branch\n"), 0o600); err != nil {
			return adapter.Response{}, err
		}
		return adapter.Response{ExternalID: id, Status: "created"}, nil
	case "outputs":
		return adapter.Response{Outputs: map[string]string{"DATABASE_URL": "postgres://fixture.invalid/db"}}, nil
	case "destroy":
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return adapter.Response{}, err
		}
		return adapter.Response{Absent: true}, nil
	default:
		return adapter.Response{}, errors.New("unsupported fixture hook")
	}
}

func main() {
	adapter.Serve(handler{})
}
