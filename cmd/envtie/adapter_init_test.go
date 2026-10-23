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

func TestSdkModuleVersion(t *testing.T) {
	prev := version
	t.Cleanup(func() { version = prev })

	// Synthetic tags only — pin behavior, not a real release string.
	version = "v9.9.9-test"
	if got := sdkModuleQuery(); got != "v9.9.9-test" {
		t.Fatalf("got %q", got)
	}
	if got := sdkRequireVersion(false); got != "v9.9.9-test" {
		t.Fatalf("require %q", got)
	}
	if got := sdkRequireVersion(true); got != "v0.0.0" {
		t.Fatalf("local require %q", got)
	}
	version = "9.9.9-test"
	if got := sdkModuleQuery(); got != "v9.9.9-test" {
		t.Fatalf("missing v → %q", got)
	}
	version = "dev"
	if got := sdkModuleQuery(); got != "latest" {
		t.Fatalf("dev → %q", got)
	}
	if got := sdkRequireVersion(false); got != "v0.0.0" {
		t.Fatalf("dev require %q", got)
	}
}

func TestInitAdapterWritesScaffold(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "adapter-demo")
	if err := initAdapter([]string{"demo", "-dir", dir, "-module", "example.com/adapter-demo", "-no-build"}); err != nil {
		t.Fatal(err)
	}
	// Flags-before-name form also works.
	dir2 := filepath.Join(root, "adapter-demo2")
	if err := initAdapter([]string{"-dir", dir2, "-module", "example.com/adapter-demo2", "-no-build", "demo"}); err != nil {
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
		case "README.md":
			if !strings.Contains(text, "envtie adapter build") {
				t.Fatalf("README missing build command: %s", text)
			}
		}
	}
	if err := initAdapter([]string{"demo", "-dir", dir, "-no-build"}); err == nil {
		t.Fatal("expected refuse without -force")
	}
	if err := initAdapter([]string{"demo", "-dir", dir, "-force", "-no-build"}); err != nil {
		t.Fatal(err)
	}
}

func TestDetectAdapterBinaryName(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "adapter-demo")
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/adapter-demo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	name, err := detectAdapterBinaryName(dir)
	if err != nil || name != "adapter-demo" {
		t.Fatalf("from module: %q %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dist", "adapter-demo.exe"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	name, err = detectAdapterBinaryName(dir)
	if err != nil || name != "adapter-demo" {
		t.Fatalf("from dist: %q %v", name, err)
	}
}

func TestGoModHasLocalReplace(t *testing.T) {
	root := t.TempDir()
	mod := "module example.com/adapter-demo\n\nrequire github.com/coddicat/envtie-core v0.0.0\n\nreplace github.com/coddicat/envtie-core => ../..\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(mod), 0644); err != nil {
		t.Fatal(err)
	}
	if !goModHasLocalReplace(root) {
		t.Fatal("expected replace")
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if goModHasLocalReplace(root) {
		t.Fatal("expected no replace")
	}
}

func TestBuildAdapterCommandFlagOrder(t *testing.T) {
	if err := buildAdapterCommand([]string{"missing-dir", "-no-test"}); err == nil {
		t.Fatal("expected missing scaffold error")
	}
}

func TestInitAdapterRejectsBadName(t *testing.T) {
	if err := initAdapter([]string{"Demo", "-no-build"}); err == nil {
		t.Fatal("expected invalid name")
	}
	if err := initAdapter([]string{"neon", "-no-build"}); err == nil {
		t.Fatal("expected reserved official name")
	}
	if err := initAdapter([]string{"demo", "-module", "bad module", "-no-build"}); err == nil {
		t.Fatal("expected invalid module")
	}
	if err := adapterCommand(nil); err == nil {
		t.Fatal("expected usage")
	}
	if err := adapterCommand([]string{"test"}); err == nil {
		t.Fatal("expected test usage")
	}
	if err := adapterCommand([]string{"build", "a", "b"}); err == nil {
		t.Fatal("expected build usage")
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
