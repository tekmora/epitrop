// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package admission

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func writeResponse(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.WriteHeader(status)
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatalf("writeResponse failed: %v", err)
	}
}

func assertRequestBasics(t *testing.T, r *http.Request, wantAuth string) {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Errorf("got method = %q, want POST", r.Method)
	}
	if r.URL.Path != "/v1/chat/completions" {
		t.Errorf("got path = %q, want /v1/chat/completions", r.URL.Path)
	}
	if r.Header.Get("Authorization") != wantAuth {
		t.Errorf("got auth = %q, want %s", r.Header.Get("Authorization"), wantAuth)
	}
	if r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("got content type = %q, want application/json", r.Header.Get("Content-Type"))
	}
}

func assertChatPayload(t *testing.T, payload chatCompletionRequest) {
	t.Helper()
	if payload.Model != "cordana/auto" {
		t.Errorf("got model = %q, want cordana/auto", payload.Model)
	}
	if payload.MaxTokens != 1 {
		t.Errorf("got max_tokens = %d, want 1", payload.MaxTokens)
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Role != "user" || payload.Messages[0].Content != "ping" {
		t.Errorf("got messages = %+v, want user ping", payload.Messages)
	}
}

func TestGatewayProber_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertRequestBasics(t, r, "Bearer my-secret-token")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		var reqPayload chatCompletionRequest
		if err := json.Unmarshal(body, &reqPayload); err != nil {
			http.Error(w, "json decode error", http.StatusBadRequest)
			return
		}
		assertChatPayload(t, reqPayload)

		w.Header().Set("x-litellm-model-id", "deployment-99")
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, http.StatusOK, `{"model":"qwen-27b","choices":[{"index":0}]}`)
	}))
	defer srv.Close()

	prober := &GatewayProber{
		BaseURL: srv.URL,
		Token:   "my-secret-token",
		Client:  srv.Client(),
	}

	res, err := prober.Probe(context.Background(), "cordana/auto")
	if err != nil {
		t.Fatalf("Probe() = %v, want nil", err)
	}

	if res.Model != "qwen-27b" {
		t.Errorf("res.Model = %q, want qwen-27b", res.Model)
	}
	if res.DeploymentID != "deployment-99" {
		t.Errorf("res.DeploymentID = %q, want deployment-99", res.DeploymentID)
	}
}

func TestGatewayProber_ServerError500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeResponse(t, w, http.StatusInternalServerError, "internal server error with details")
	}))
	defer srv.Close()

	prober := &GatewayProber{
		BaseURL: srv.URL,
		Token:   "secret",
		Client:  srv.Client(),
	}

	_, err := prober.Probe(context.Background(), "cordana/auto")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("Probe() error = %v, want naming HTTP 500", err)
	}
	if strings.Contains(err.Error(), "with details") {
		t.Fatalf("Probe() error leaked response body: %q", err.Error())
	}
}

func TestGatewayProber_MissingModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, http.StatusOK, `{"choices":[{"index":0}]}`)
	}))
	defer srv.Close()

	prober := &GatewayProber{
		BaseURL: srv.URL,
		Token:   "secret",
		Client:  srv.Client(),
	}

	_, err := prober.Probe(context.Background(), "cordana/auto")
	if err == nil || !strings.Contains(err.Error(), "missing model") {
		t.Fatalf("Probe() error = %v, want missing model error", err)
	}
}

func TestGatewayProber_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, http.StatusOK, `{"model":"m1","choices":[]}`)
	}))
	defer srv.Close()

	prober := &GatewayProber{
		BaseURL: srv.URL,
		Token:   "secret",
		Client:  srv.Client(),
	}

	_, err := prober.Probe(context.Background(), "cordana/auto")
	if err == nil || !strings.Contains(err.Error(), "choices array is empty") {
		t.Fatalf("Probe() error = %v, want choices array is empty error", err)
	}
}

func TestGatewayProber_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, http.StatusOK, `{not valid json`)
	}))
	defer srv.Close()

	prober := &GatewayProber{
		BaseURL: srv.URL,
		Token:   "secret",
		Client:  srv.Client(),
	}

	_, err := prober.Probe(context.Background(), "cordana/auto")
	if err == nil {
		t.Fatal("Probe() = nil, want error on malformed JSON")
	}
}

func assertTokenNotLeaked(t *testing.T, err error, token string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestGatewayProber_TokenNeverLeaks_StatusCodes(t *testing.T) {
	const secret = "SECRET-TOKEN-123"
	statuses := []int{
		http.StatusInternalServerError,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusBadGateway,
	}
	for _, status := range statuses {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeResponse(t, w, status, "unauthorized SECRET-TOKEN-123 in body")
		}))
		prober := &GatewayProber{BaseURL: srv.URL, Token: secret, Client: srv.Client()}
		_, err := prober.Probe(context.Background(), "cordana/auto")
		srv.Close()
		assertTokenNotLeaked(t, err, secret)
	}
}

func TestGatewayProber_TokenNeverLeaks_DecodeAndNetwork(t *testing.T) {
	const secret = "SECRET-TOKEN-123"

	// Malformed JSON body
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeResponse(t, w, http.StatusOK, "{invalid-json-SECRET-TOKEN-123")
	}))
	prober1 := &GatewayProber{BaseURL: srv1.URL, Token: secret, Client: srv1.Client()}
	_, err1 := prober1.Probe(context.Background(), "cordana/auto")
	srv1.Close()
	assertTokenNotLeaked(t, err1, secret)

	// Missing model body
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeResponse(t, w, http.StatusOK, `{"choices":[{"index":0}]}`)
	}))
	prober2 := &GatewayProber{BaseURL: srv2.URL, Token: secret, Client: srv2.Client()}
	_, err2 := prober2.Probe(context.Background(), "cordana/auto")
	srv2.Close()
	assertTokenNotLeaked(t, err2, secret)

	// Closed server
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv3.URL
	srv3.Close()
	prober3 := &GatewayProber{BaseURL: closedURL, Token: secret, Client: http.DefaultClient}
	_, err3 := prober3.Probe(context.Background(), "cordana/auto")
	assertTokenNotLeaked(t, err3, secret)

	// Context canceled
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv4.Close()
	prober4 := &GatewayProber{BaseURL: srv4.URL, Token: secret, Client: srv4.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err4 := prober4.Probe(ctx, "cordana/auto")
	assertTokenNotLeaked(t, err4, secret)
}

func TestGatewayProber_NilContext(t *testing.T) {
	prober := &GatewayProber{BaseURL: "http://localhost:8080", Token: "tok"}
	var nilCtx context.Context
	_, err := prober.Probe(nilCtx, "cordana/auto")
	if err == nil {
		t.Fatal("Probe(nil) = nil, want error")
	}
}

// TestGatewayProber_TokenRedactedFromEndpointError covers the second barrier:
// a base URL that carries the token puts it into the transport error, and
// the prober must redact it before returning.
func TestGatewayProber_TokenRedactedFromEndpointError(t *testing.T) {
	const secret = "s3cr3t-tok-path"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()
	prober := &GatewayProber{BaseURL: closedURL + "/" + secret, Token: secret, Client: http.DefaultClient}
	_, err := prober.Probe(context.Background(), "cordana/auto")
	if err == nil {
		t.Fatal("Probe(closed server) = nil, want error")
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("Probe() error = %v, want the endpoint token redacted", err)
	}
	assertTokenNotLeaked(t, err, secret)
}

func TestGatewayProber_ServerErrorOverCapNamesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, http.StatusServiceUnavailable, strings.Repeat("x", maxProbeResponseBytes+1))
	}))
	defer srv.Close()
	prober := &GatewayProber{BaseURL: srv.URL, Token: "secret", Client: srv.Client()}
	_, err := prober.Probe(context.Background(), "cordana/auto")
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") || strings.Contains(err.Error(), "exceeds byte cap") {
		t.Fatalf("Probe() error = %v", err)
	}
}
