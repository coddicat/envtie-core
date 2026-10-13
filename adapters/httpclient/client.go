package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var Client = &http.Client{Timeout: 30 * time.Second}

func JSON(ctx context.Context, method, url, auth string, body any, target any) (int, error) {
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, errors.New("provider request failed")
		}
		input = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, input)
	if err != nil {
		return 0, errors.New("provider request failed")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "EnvTie.dev/1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := Client.Do(req)
	if err != nil {
		return 0, errors.New("provider request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Keep a small provider response only for local classification. Error()
		// never returns it, so adapter/process/control-plane diagnostics cannot
		// accidentally include provider text or echoed request values.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return resp.StatusCode, &StatusError{Status: resp.StatusCode, body: body}
	}
	if target != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
			return resp.StatusCode, errors.New("invalid provider response")
		}
	}
	return resp.StatusCode, nil
}

// StatusClass maps an HTTP status to a stable category. Classification uses
// only the status code and never a response body.
func StatusClass(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "auth"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusPaymentRequired:
		return "quota"
	case http.StatusTooManyRequests:
		return "rate_limit"
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid"
	default:
		if status >= 500 && status <= 599 {
			return "unavailable"
		}
		return "error"
	}
}

// StatusError is a provider HTTP failure. It carries the status and no response body.
type StatusError struct {
	Status int
	body   []byte
}

func (e *StatusError) Error() string { return "provider returned HTTP error" }

func (e *StatusError) Class() string {
	if e == nil {
		return "error"
	}
	return StatusClass(e.Status)
}

// ErrorContains lets an adapter recognize a documented, non-secret provider
// condition without exposing the response body through the error contract.
func ErrorContains(err error, text string) bool {
	var status *StatusError
	if !errors.As(err, &status) || text == "" {
		return false
	}
	return bytes.Contains(bytes.ToLower(status.body), bytes.ToLower([]byte(text)))
}

// ProviderError is a form-encoded provider failure. Its bounded body stays
// private and is available only to local classification helpers.
type ProviderError struct {
	Status int
	body   []byte
}

func (e *ProviderError) Error() string { return "provider returned HTTP error" }

func (e *ProviderError) Class() string {
	if e == nil {
		return "error"
	}
	return StatusClass(e.Status)
}

// DecodeErrorBody lets a provider adapter classify a structured error locally
// without making the provider body part of the exported error value.
func DecodeErrorBody(err error, target any) bool {
	var provider *ProviderError
	if !errors.As(err, &provider) || target == nil {
		return false
	}
	return json.Unmarshal(provider.body, target) == nil
}

// Form sends an application/x-www-form-urlencoded request, as used by Stripe's v1 API.
func Form(ctx context.Context, method, endpoint, auth string, values url.Values, headers map[string]string, target any) (int, error) {
	var input io.Reader
	if values != nil {
		input = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, input)
	if err != nil {
		return 0, errors.New("provider request failed")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "EnvTie.dev/1")
	if values != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := Client.Do(req)
	if err != nil {
		return 0, errors.New("provider request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return resp.StatusCode, &ProviderError{Status: resp.StatusCode, body: body}
	}
	if target != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
			return resp.StatusCode, errors.New("invalid provider response")
		}
	}
	return resp.StatusCode, nil
}
