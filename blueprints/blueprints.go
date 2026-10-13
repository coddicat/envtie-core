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
		"neon.yaml",
		"stripe-webhook.yaml",
		"referenced-neon.yaml",
	}
}

// Template describes one dashboard starter backed by an embedded YAML file.
type Template struct {
	ID          string
	Label       string
	Description string
	Body        string
}

// Dashboard returns the canonical dashboard starter catalog.
func Dashboard() []Template {
	specs := []struct {
		id, label, description, file string
	}{
		{"postgres-redis", "Neon + Upstash", "A Neon branch and an Upstash database for each environment. Connection values are written to that environment's dotenv file.", "neon-upstash-local.yaml"},
		{"postgres", "Neon", "A Neon branch for each environment, removed after 3 days.", "neon.yaml"},
		{"stripe-webhook", "Stripe webhook", "A Stripe test-mode webhook endpoint per environment; the signing secret is delivered once, at creation.", "stripe-webhook.yaml"},
		{"reference", "Existing Neon branch", "Bind a Neon branch that already exists. EnvTie.dev reads it and never changes or deletes it. The database and role on that branch are still required.", "referenced-neon.yaml"},
	}
	result := make([]Template, 0, len(specs))
	for _, spec := range specs {
		body, err := Get(spec.file)
		if err != nil {
			panic(err)
		}
		result = append(result, Template{ID: spec.id, Label: spec.label, Description: spec.description, Body: body})
	}
	return result
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
