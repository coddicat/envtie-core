package core

import (
	"strings"
	"testing"
	"time"
)

func fixture() Blueprint {
	return Blueprint{Name: "preview", Components: []Component{{ID: "database", Adapter: "neon@1", Executable: "adapter-neon", SHA256: strings.Repeat("a", 64), Ownership: Managed}, {ID: "cache", Adapter: "upstash@1", Executable: "adapter-upstash", SHA256: strings.Repeat("b", 64), Ownership: Managed, DependsOn: []string{"database"}}}, Policy: Policy{ExpiresIn: "7d"}}
}

func TestDigest(t *testing.T) {
	a, b := fixture(), fixture()
	b.Bindings = []Binding{}
	if a.Digest() != b.Digest() {
		t.Fatal("absent and empty bindings must share a digest")
	}
	b.Policy.ExpiresIn = "3d"
	if a.Digest() == b.Digest() {
		t.Fatal("different content must change the digest")
	}
	p, _ := NewPlan(a, "pr-1", nil, time.Now())
	if p.BlueprintDigest != a.Digest() {
		t.Fatal("plans must record the blueprint digest")
	}
}

func TestOrderAndExpiry(t *testing.T) {
	b := fixture()
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	order, err := b.Order()
	if err != nil {
		t.Fatal(err)
	}
	if order[0].ID != "database" || order[1].ID != "cache" {
		t.Fatalf("order: %+v", order)
	}
	p, err := NewPlan(b, "pr-481", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.ResourceExpiresAt == nil || p.ResourceExpiresAt.Sub(p.CreatedAt) != 7*24*time.Hour {
		t.Fatalf("expiry: %+v", p.ResourceExpiresAt)
	}
}
func TestCycleRejected(t *testing.T) {
	b := fixture()
	b.Components[0].DependsOn = []string{"cache"}
	if err := b.Validate(); err == nil {
		t.Fatal("cycle accepted")
	}
}
func TestSecretsRejected(t *testing.T) {
	b := fixture()
	b.Components[0].Config = map[string]string{"api_key": "plaintext"}
	if err := b.Validate(); err == nil {
		t.Fatal("secret-shaped config accepted")
	}
}
func TestReferencedNeedsID(t *testing.T) {
	b := fixture()
	b.Components[0].Ownership = Referenced
	if err := b.Validate(); err == nil {
		t.Fatal("referenced resource without id accepted")
	}
}

func TestGitHubBindingsRequireTheGitHubProvider(t *testing.T) {
	b := Blueprint{Name: "preview", Components: []Component{
		{ID: "db", Adapter: "neon@1", Ownership: Managed, Config: map[string]string{"project_id": "p", "database_name": "d", "role_name": "r"}},
	}, Bindings: []Binding{{From: "db.DATABASE_URL", To: "github.environment.secret.DATABASE_URL"}}}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "github@1") {
		t.Fatal(err)
	}
	b.Components = append(b.Components, Component{ID: "github", Adapter: "github@1", Ownership: Managed, Config: map[string]string{"deployment": "true", "repository": "acme/app"}})
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestVercelEnvBindingsRequireTheVercelProvider(t *testing.T) {
	b := Blueprint{Name: "preview", Components: []Component{
		{ID: "db", Adapter: "neon@1", Ownership: Managed, Config: map[string]string{"project_id": "p", "database_name": "d", "role_name": "r"}},
	}, Bindings: []Binding{{From: "db.DATABASE_URL", To: "vercel.env.DATABASE_URL"}}}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "vercel@1") {
		t.Fatal(err)
	}
	// Blob alone does not satisfy vercel.env.* — Hosting is required.
	b.Components = append(b.Components, Component{
		ID: "blob", Adapter: "vercel@1", Ownership: Managed,
		Config: map[string]string{"product": "blob", "access": "private"},
	})
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "Hosting") {
		t.Fatal(err)
	}
	b.Components = append(b.Components, Component{
		ID: "host", Adapter: "vercel@1", Ownership: Managed, DependsOn: []string{"db", "blob"},
		Config: map[string]string{"project_id": "prj_app"},
	})
	b.Bindings = append(b.Bindings, Binding{From: "host.VERCEL_URL", To: "dotenv.VERCEL_URL"})
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestVercelEnvBindingsRequireDependsOn(t *testing.T) {
	b := Blueprint{Name: "preview", Components: []Component{
		{ID: "db", Adapter: "neon@1", Ownership: Managed, Config: map[string]string{"project_id": "p", "database_name": "d", "role_name": "r"}},
		{ID: "host", Adapter: "vercel@1", Ownership: Managed, Config: map[string]string{"project_id": "prj_app"}},
	}, Bindings: []Binding{
		{From: "db.DATABASE_URL", To: "vercel.env.DATABASE_URL"},
		{From: "host.VERCEL_URL", To: "dotenv.VERCEL_URL"},
	}}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "order between") {
		t.Fatal(err)
	}
	b.Components[1].DependsOn = []string{"db"}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestVercelEnvBindingsAllowSourceAfterVercel(t *testing.T) {
	b := Blueprint{Name: "preview", Components: []Component{
		{ID: "host", Adapter: "vercel@1", Ownership: Managed, Config: map[string]string{"project_id": "prj_app"}},
		{ID: "stripe_webhook", Adapter: "stripe@1", Ownership: Managed, DependsOn: []string{"host"}, Config: map[string]string{
			"host": "{host.VERCEL_URL}", "path": "/api/webhooks/stripe", "events": "invoice.paid",
		}},
	}, Bindings: []Binding{
		{From: "stripe_webhook.STRIPE_WEBHOOK_SECRET", To: "vercel.env.STRIPE_WEBHOOK_SECRET"},
		{From: "stripe_webhook.STRIPE_WEBHOOK_ENDPOINT_ID", To: "vercel.env.STRIPE_WEBHOOK_ENDPOINT_ID"},
		{From: "host.VERCEL_URL", To: "dotenv.VERCEL_URL"},
	}}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCustomAdapterRequiresExplicitSource(t *testing.T) {
	component := Component{ID: "private", Adapter: "example@1", Ownership: Managed}
	b := Blueprint{Name: "custom", Components: []Component{component}}
	if err := b.Validate(); err == nil {
		t.Fatal("custom adapter without source accepted")
	}
	b.Components[0].Source = &AdapterSource{
		Kind: "registry", Registry: "https://adapters.example", Publisher: "acme", Version: "1.2.3",
	}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	b.Components[0].Source = nil
	b.Components[0].Executable = "./adapter-example"
	b.Components[0].SHA256 = strings.Repeat("a", 64)
	if err := b.Validate(); err != nil {
		t.Fatal("local custom adapter escape hatch:", err)
	}
}
