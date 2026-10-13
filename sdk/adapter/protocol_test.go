package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaseEnvironmentExcludesProviderCredentials(t *testing.T) {
	t.Setenv("NEON_API_KEY", "secret-fixture")
	for _, entry := range baseEnvironment() {
		if strings.Contains(entry, "secret-fixture") {
			t.Fatal("provider secret inherited by adapter manifest process")
		}
	}
}

func TestErrorResponseKeepsOnlyStableCode(t *testing.T) {
	const leak = "postgresql://user:secret@db.example/app https://console.neon.tech/api BODY-MARKER"
	resp := errorResponse(fmt.Errorf("%s: %w", leak, NewError("neon_quota")))
	if resp.ErrorCode != "neon_quota" || resp.Outputs != nil || resp.ExternalID != "" {
		t.Fatalf("%+v", resp)
	}
	encoded, _ := json.Marshal(resp)
	assertNoLeak(t, string(encoded))
	bad := errorResponse(fmt.Errorf("%s", leak))
	if bad.ErrorCode != "adapter_failed" {
		t.Fatalf("uncoded: %+v", bad)
	}
	rejected := errorResponse(NewError("https://user:secret@example.com/BODY-MARKER"))
	if rejected.ErrorCode != "adapter_failed" {
		t.Fatalf("unsafe code: %+v", rejected)
	}
	assertNoLeak(t, string(jsonMust(rejected)))
}

func TestFailureDropsUnsafeDetails(t *testing.T) {
	const leak = "postgresql://user:secret@db.example/app https://console.neon.tech/api BODY-MARKER"
	err := WithPlace("db", "apply", fmt.Errorf("%s: %w", leak, NewError("neon_quota")))
	var failure *Failure
	var coded *CodedError
	if !errors.As(err, &failure) || failure.Code != "neon_quota" || failure.Component != "db" || failure.Hook != "apply" {
		t.Fatalf("placed: %v", err)
	}
	if !errors.As(err, &coded) || coded.Code != "neon_quota" || err.Error() != "db apply: neon_quota" {
		t.Fatalf("code: %v", err)
	}
	assertNoLeak(t, err.Error())

	unsafe := WithPlace("../secret", "https://secret", errors.New(leak))
	if unsafe.Error() != "adapter_failed" {
		t.Fatalf("unsafe place: %v", unsafe)
	}
	assertNoLeak(t, unsafe.Error())
	direct := &Failure{Code: "https://user:secret@example.com/BODY-MARKER", Component: "DB", Hook: "Apply"}
	if direct.Error() != "adapter_failed" {
		t.Fatalf("direct: %v", direct)
	}
	assertNoLeak(t, direct.Error())
	if errors.Unwrap(direct).Error() != "adapter_failed" {
		t.Fatal("unwrap kept an unsafe code")
	}
}

func TestCallPreservesStableCodeWithoutProviderDetails(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "diagchild.exe")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/diagchild")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build child: %v\n%s", err, out)
	}
	sum := sha256.New()
	f, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(sum, f); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	exe := Executable{Path: bin, SHA256: hex.EncodeToString(sum.Sum(nil))}
	for _, tc := range []struct{ mode, want string }{
		{mode: "quota", want: "neon_quota"},
		{mode: "wrapped", want: "neon_quota"},
		{mode: "bad", want: "adapter_failed"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			_, err := exe.Call(context.Background(), Request{
				Protocol:       ProtocolVersion,
				Hook:           "apply",
				Environment:    "preview",
				Component:      "db",
				Config:         map[string]string{"mode": tc.mode},
				IdempotencyKey: "plan/db",
			})
			var failure *Failure
			var coded *CodedError
			if !errors.As(err, &failure) || failure.Code != tc.want || failure.Component != "db" || failure.Hook != "apply" {
				t.Fatalf("failure: %v", err)
			}
			if !errors.As(err, &coded) || coded.Code != tc.want || err.Error() != "db apply: "+tc.want {
				t.Fatalf("code: %v", err)
			}
			assertNoLeak(t, err.Error())
		})
	}
}

func jsonMust(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func assertNoLeak(t *testing.T, text string) {
	t.Helper()
	for _, leak := range []string{"BODY-MARKER", "postgresql://", "https://", "user:secret", "db.example", "console.neon.tech"} {
		if strings.Contains(text, leak) {
			t.Fatalf("text %q contains %q", text, leak)
		}
	}
}
