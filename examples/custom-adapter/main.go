// A minimal private adapter. Build it, pin its SHA-256 in a blueprint, and use
// exactly the same runner path as the official adapters.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	"github.com/coddicat/envtie-core/sdk/adapter"
)

type handler struct{}

var safe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func (handler) Manifest() adapter.Manifest {
	return adapter.Manifest{Name: "example", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}, CredentialEnv: []string{"ENVTIE_EXAMPLE_STATE_DIR", "ENVTIE_EXAMPLE_SECRET"}, Outputs: []adapter.Field{{Name: "EXAMPLE_VALUE", Sensitivity: "SECRET"}}}
}

func (handler) Handle(_ context.Context, r adapter.Request) (adapter.Response, error) {
	if !safe.MatchString(r.Environment) || !safe.MatchString(r.Component) {
		return adapter.Response{}, errors.New("invalid identity")
	}
	directory := os.Getenv("ENVTIE_EXAMPLE_STATE_DIR")
	if directory == "" {
		return adapter.Response{}, errors.New("state directory required")
	}
	id := r.Environment + "-" + r.Component
	if r.ExternalID != "" {
		if r.ExternalID != id {
			return adapter.Response{}, errors.New("resource identity mismatch")
		}
	}
	path := filepath.Join(directory, id+".txt")
	switch r.Hook {
	case "read", "validate":
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return adapter.Response{Absent: true}, nil
		} else if err != nil {
			return adapter.Response{}, err
		}
		return adapter.Response{ExternalID: id, Status: "ready"}, nil
	case "apply":
		if err := os.MkdirAll(directory, 0700); err != nil {
			return adapter.Response{}, err
		}
		if err := os.WriteFile(path, []byte("managed by EnvTie.dev\n"), 0600); err != nil {
			return adapter.Response{}, err
		}
		return adapter.Response{ExternalID: id, Status: "created"}, nil
	case "outputs":
		return adapter.Response{Outputs: map[string]string{"EXAMPLE_VALUE": os.Getenv("ENVTIE_EXAMPLE_SECRET")}}, nil
	case "destroy":
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return adapter.Response{}, err
		}
		return adapter.Response{Absent: true}, nil
	}
	return adapter.Response{}, errors.New("unsupported hook")
}

func main() { adapter.Serve(handler{}) }
