// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package releasewatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
)

func TestFetchSourceNon200OverCapReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		if _, err := io.WriteString(w, strings.Repeat("x", maxSourceResponseBytes+1)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()
	client, err := NewClient(ClientOptions{GitHubBase: srv.URL, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = FetchSource(context.Background(), client, config.ReleaseWatchSource{GitHub: "owner/repo"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") || strings.Contains(err.Error(), "too large") {
		t.Fatalf("fetch error = %v, want HTTP 503", err)
	}
}
