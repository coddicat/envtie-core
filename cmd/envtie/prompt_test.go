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

func TestMaskedStepPrintsStarsAndHonorsBackspace(t *testing.T) {
	buf, echo, done, interrupt := maskedStep(nil, 'a')
	if string(buf) != "a" || echo != "*" || done || interrupt {
		t.Fatalf("type: %q %q %v %v", buf, echo, done, interrupt)
	}
	buf, echo, done, interrupt = maskedStep(buf, 127)
	if len(buf) != 0 || echo != "\b \b" || done || interrupt {
		t.Fatalf("backspace: %q %q %v %v", buf, echo, done, interrupt)
	}
	buf, echo, done, interrupt = maskedStep([]byte("ab"), '\r')
	if string(buf) != "ab" || echo != "\r\n" || !done || interrupt {
		t.Fatalf("enter: %q %q %v %v", buf, echo, done, interrupt)
	}
	_, _, done, interrupt = maskedStep([]byte("a"), 3)
	if done || !interrupt {
		t.Fatal("ctrl+c should interrupt")
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
