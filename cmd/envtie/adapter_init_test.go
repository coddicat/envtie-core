package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvPrefix(t *testing.T) {
	if got := envPrefix("mail-trap"); got != "MAIL_TRAP" {
		t.Fatalf("got %q", got)
	}
}

func TestInitAdapterWritesScaffold(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "adapter-demo")
	if err := initAdapter([]string{"demo", "-dir", dir, "-module", "example.com/adapter-demo"}); err != nil {
		t.Fatal(err)
	}
	// Flags-before-name form also works.
	dir2 := filepath.Join(root, "adapter-demo2")
	if err := initAdapter([]string{"-dir", dir2, "-module", "example.com/adapter-demo2", "demo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "main.go")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.go", "main_test.go", "go.mod", "README.md", "blueprint.example.yaml", ".gitignore"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := string(data)
		switch name {
		case "main.go":
			if !strings.Contains(text, `Name:         "demo"`) {
				t.Fatalf("main.go missing adapter name: %s", text)
			}
			if !strings.Contains(text, "DEMO_API_TOKEN") || !strings.Contains(text, "DEMO_VALUE") {
				t.Fatalf("main.go missing env/output names: %s", text)
			}
		case "go.mod":
			if !strings.Contains(text, "module example.com/adapter-demo") {
				t.Fatalf("go.mod: %s", text)
			}
			if !strings.Contains(text, "github.com/coddicat/envtie-core") {
				t.Fatalf("go.mod missing require: %s", text)
			}
		case "blueprint.example.yaml":
			if !strings.Contains(text, "adapter: demo@1") || !strings.Contains(text, "DEMO_VALUE") {
				t.Fatalf("blueprint: %s", text)
			}
		}
	}
	if err := initAdapter([]string{"demo", "-dir", dir}); err == nil {
		t.Fatal("expected refuse without -force")
	}
	if err := initAdapter([]string{"demo", "-dir", dir, "-force"}); err != nil {
		t.Fatal(err)
	}
}

func TestInitAdapterRejectsBadName(t *testing.T) {
	if err := initAdapter([]string{"Demo"}); err == nil {
		t.Fatal("expected invalid name")
	}
	if err := initAdapter([]string{"neon"}); err == nil {
		t.Fatal("expected reserved official name")
	}
	if err := initAdapter([]string{"demo", "-module", "bad module"}); err == nil {
		t.Fatal("expected invalid module")
	}
	if err := adapterCommand(nil); err == nil {
		t.Fatal("expected usage")
	}
	if err := adapterCommand([]string{"test"}); err == nil {
		t.Fatal("expected test usage")
	}
}

func TestFindLocalEnvtieCore(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// This test file lives in cmd/envtie under the envtie-core module.
	rel := findLocalEnvtieCore(filepath.Join(wd, "testdata-scaffold"))
	if rel == "" {
		t.Fatal("expected replace path when running inside envtie-core")
	}
	if strings.Contains(rel, `\`) {
		t.Fatalf("replace path should be slash-form: %q", rel)
	}
}
