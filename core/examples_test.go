package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalBlueprints(t *testing.T) {
	schema, err := os.ReadFile(filepath.Join("..", "schemas", "blueprint.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), `"pattern": "^[a-fA-F0-9]{64}$"`) {
		t.Fatal("blueprint schema pin pattern changed; update the preflight docs and examples")
	}

	cases := []struct {
		name     string
		bindings []Binding
	}{
		{
			name: "neon-upstash-local.yaml",
			bindings: []Binding{
				{From: "postgres.DATABASE_URL", To: "dotenv.DATABASE_URL"},
				{From: "redis.UPSTASH_REDIS_REST_URL", To: "dotenv.UPSTASH_REDIS_REST_URL"},
				{From: "redis.UPSTASH_REDIS_REST_TOKEN", To: "dotenv.UPSTASH_REDIS_REST_TOKEN"},
			},
		},
		{
			name: "neon-upstash-windows.yaml",
			bindings: []Binding{
				{From: "postgres.DATABASE_URL", To: "dotenv.DATABASE_URL"},
				{From: "redis.UPSTASH_REDIS_REST_URL", To: "dotenv.UPSTASH_REDIS_REST_URL"},
				{From: "redis.UPSTASH_REDIS_REST_TOKEN", To: "dotenv.UPSTASH_REDIS_REST_TOKEN"},
			},
		},
		{
			name: "neon-upstash-ci.yaml",
			bindings: []Binding{
				{From: "postgres.DATABASE_URL", To: "github.environment.secret.DATABASE_URL"},
				{From: "redis.UPSTASH_REDIS_REST_URL", To: "github.environment.variable.UPSTASH_REDIS_REST_URL"},
				{From: "redis.UPSTASH_REDIS_REST_TOKEN", To: "github.environment.secret.UPSTASH_REDIS_REST_TOKEN"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join("..", "blueprints", tc.name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(raw)
			if strings.Contains(text, "://") {
				t.Fatal("example contains a URL or secret-shaped value")
			}
			blueprint, err := ParseBlueprint(raw)
			if err != nil {
				t.Fatal(err)
			}
			if blueprint.Name != "consumer-bench" {
				t.Fatalf("name %s", blueprint.Name)
			}
			if blueprint.Policy.ExpiresIn != "1d" || blueprint.Policy.Protect {
				t.Fatalf("policy %+v", blueprint.Policy)
			}
			wantComponents := 2
			if HasGitHubProvider(blueprint) {
				wantComponents = 3
			}
			if len(blueprint.Components) != wantComponents || strings.Contains(tc.name, "-ci") != HasGitHubProvider(blueprint) {
				t.Fatalf("components %d", len(blueprint.Components))
			}
			for _, component := range blueprint.Components {
				if component.Executable != "" || component.SHA256 != "" {
					t.Fatalf("official adapter exposes runtime fields: %+v", component)
				}
			}
			if blueprint.Components[0].Config["project_id"] != "REPLACE_WITH_NEON_PROJECT_ID" ||
				blueprint.Components[0].Config["database_name"] != "REPLACE_WITH_NEON_DATABASE" ||
				blueprint.Components[0].Config["role_name"] != "REPLACE_WITH_NEON_ROLE" {
				t.Fatalf("neon config %#v", blueprint.Components[0].Config)
			}
			if blueprint.Components[1].Config["platform"] != "aws" || blueprint.Components[1].Config["primary_region"] != "us-east-1" {
				t.Fatalf("upstash config %#v", blueprint.Components[1].Config)
			}
			if len(blueprint.Bindings) != len(tc.bindings) {
				t.Fatalf("bindings %#v", blueprint.Bindings)
			}
			for i, binding := range tc.bindings {
				if blueprint.Bindings[i] != binding {
					t.Fatalf("binding %d %#v", i, blueprint.Bindings[i])
				}
			}
		})
	}
}
