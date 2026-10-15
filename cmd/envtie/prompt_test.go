package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestConnectionValues(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !validPublicKey(base64.StdEncoding.EncodeToString(pub)) || validPublicKey("not-a-key") {
		t.Fatal("public key check")
	}
	if !secureAPIURL("https://envtie.example") || !secureAPIURL("http://127.0.0.1:8080") || secureAPIURL("http://envtie.example") {
		t.Fatal("API URL check")
	}
}

func TestAcceptableCredentialRejectsEmptyAndMultilineValues(t *testing.T) {
	if !acceptableCredential("ep_rt_example") {
		t.Fatal("expected a normal value")
	}
	for _, value := range []string{"", "   ", "line\nbreak", "line\rbreak", "has\x00null", string(make([]byte, 4097))} {
		if acceptableCredential(value) {
			t.Fatalf("accepted %q", value)
		}
	}
}
