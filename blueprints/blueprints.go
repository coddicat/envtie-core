// Package blueprints embeds the canonical EnvTie blueprint templates.
package blueprints

import (
	"embed"
	"fmt"
)

//go:embed *.yaml
var files embed.FS

// Names returns the stable names of the public templates.
func Names() []string {
	return []string{
		"starter.yaml",
		"neon-upstash-local.yaml",
		"neon-upstash-windows.yaml",
		"neon-upstash-ci.yaml",
	}
}

// Get returns one canonical YAML template.
func Get(name string) (string, error) {
	body, err := files.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("blueprint template %q: %w", name, err)
	}
	return string(body), nil
}

// Starter returns the setup pull request template.
func Starter() string {
	body, err := Get("starter.yaml")
	if err != nil {
		panic(err)
	}
	return body
}
