package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

const leakBody = `BODY-MARKER postgresql://role:secret@ep.neon.tech/db https://console.neon.tech/api quota exceeded`

func TestStatusClassUsesStatusOnly(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{401, "auth"},
		{403, "auth"},
		{404, "not_found"},
		{402, "quota"},
		{429, "rate_limit"},
		{400, "invalid"},
		{422, "invalid"},
		{500, "unavailable"},
		{503, "unavailable"},
		{409, "error"},
		{200, "error"},
		{0, "error"},
	}
	for _, tc := range cases {
		if got := StatusClass(tc.status); got != tc.want {
			t.Fatalf("status %d: got %s, want %s", tc.status, got, tc.want)
		}
	}
}

func TestJSONErrorExposesStatusWithoutBody(t *testing.T) {
	old := Client
	defer func() { Client = old }()
	Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == "https://provider.example/transport" {
			return nil, errors.New(`Get "https://provider.example/secret": ` + leakBody)
		}
		return &http.Response{
			StatusCode: 400,
			Body:       io.NopCloser(strings.NewReader(leakBody)),
			Header:     make(http.Header),
		}, nil
	})}
	_, err := JSON(context.Background(), "GET", "https://provider.example/transport", "Bearer napi-fixture-key", nil, nil)
	if err == nil || err.Error() != "provider request failed" {
		t.Fatalf("transport: %v", err)
	}
	assertNoLeak(t, err)
	if _, ok := err.(*StatusError); ok {
		t.Fatal("transport failure must not look like an HTTP status")
	}

	status, err := JSON(context.Background(), "POST", "https://provider.example/denied", "Bearer napi-fixture-key", map[string]string{"name": "preview"}, nil)
	se, ok := err.(*StatusError)
	if status != 400 || !ok || se.Status != 400 || se.Class() != "invalid" {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if _, ok := err.(*ProviderError); ok {
		t.Fatal("JSON error retained a provider body")
	}
	if strings.Contains(fmt.Sprintf("%#v", err), "BODY-MARKER") {
		t.Fatal("status error value contains the response body")
	}
	assertNoLeak(t, err)
}

func TestProviderErrorClassIgnoresBody(t *testing.T) {
	err := &ProviderError{Status: 401, body: []byte(leakBody)}
	if err.Error() != "provider returned HTTP error" || err.Class() != "auth" {
		t.Fatalf("class=%s err=%s", err.Class(), err.Error())
	}
	assertNoLeak(t, err)
	serialized, marshalErr := json.Marshal(err)
	if marshalErr != nil || strings.Contains(string(serialized), "BODY-MARKER") {
		t.Fatalf("provider body serialized: %s %v", serialized, marshalErr)
	}
	var decoded struct {
		Marker string `json:"marker"`
	}
	err.body = []byte(`{"marker":"local"}`)
	if !DecodeErrorBody(err, &decoded) || decoded.Marker != "local" {
		t.Fatal("local body classification failed")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func assertNoLeak(t *testing.T, err error) {
	t.Helper()
	seen := map[error]bool{}
	for err != nil && !seen[err] {
		seen[err] = true
		text := err.Error()
		for _, leak := range []string{"BODY-MARKER", "postgresql://", "https://", "napi-fixture-key", "quota exceeded", "role:secret", "provider.example"} {
			if strings.Contains(text, leak) {
				t.Fatalf("error %q contains %q", text, leak)
			}
		}
		err = errors.Unwrap(err)
	}
}
