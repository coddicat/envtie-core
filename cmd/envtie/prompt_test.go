package main

import "testing"

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
