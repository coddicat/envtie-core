package main

import (
	"testing"

	"github.com/coddicat/envtie-core/adapters/mailtrap"
)

func TestExampleWiresOfficialHandler(t *testing.T) {
	m := mailtrap.Handler{}.Manifest()
	if m.Name != "mailtrap" {
		t.Fatalf("%+v", m)
	}
}
