//go:build schemasrefresh

// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateSourceURLRefusesUserInfo(t *testing.T) {
	cases := []string{"https://user:s3cret@example.invalid/p", "https://user@example.invalid/p", "http://user:s3cret@example.invalid/p", "https://user:s3cret@exa mple.invalid/"}
	for _, raw := range cases {
		err := validateSourceURL(schemaSource{ID: "source-id", URLTemplate: raw})
		if err == nil || !strings.Contains(err.Error(), "source-id") || strings.Contains(err.Error(), "s3cret") {
			t.Fatalf("error = %v", err)
		}
		if !strings.Contains(raw, "exa mple") && !strings.Contains(err.Error(), "example.invalid") {
			t.Fatalf("missing host: %v", err)
		}
	}
}

func TestCheckHTTPSRedirectRefusesUserInfo(t *testing.T) {
	for _, raw := range []string{"https://user:s3cret@example.invalid/", "http://user:s3cret@example.invalid/"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = checkHTTPSRedirect(req, nil)
		if err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Fatalf("redirect error = %v", err)
		}
	}
}

func TestFetchPinnedStatusErrorRedactsPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	raw := strings.Replace(srv.URL, "http://", "http://user:s3cret@", 1)
	_, err := fetchPinned(context.Background(), schemaSource{ID: "source-id", URLTemplate: raw})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("fetch error = %v", err)
	}
}

func TestInvalidPasswordEscapeRedacted(t *testing.T) {
	source := schemaSource{ID: "source-id", URLTemplate: "https://user:p4ss%zzw0rd@example.invalid/x"}
	for name, check := range map[string]func() error{
		"validate": func() error { return validateSourceURL(source) },
		"fetch":    func() error { _, err := fetchPinned(context.Background(), source); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := check()
			if err == nil {
				t.Fatal("expected invalid URL error")
			}
			for _, secret := range []string{"p4ss", "w0rd", "zz"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error exposes %q: %v", secret, err)
				}
			}
		})
	}
}
