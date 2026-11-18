//go:build schemasrefresh

// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRouterFragmentReflectsPinnedSchemaRename(t *testing.T) {
	original := syntheticOpenRouterDocument("context_length")
	renamed := syntheticOpenRouterDocument("context_window")

	gotOriginal, err := openRouterFragment(original)
	if err != nil {
		t.Fatalf("openRouterFragment(original) = %v", err)
	}
	gotRenamed, err := openRouterFragment(renamed)
	if err != nil {
		t.Fatalf("openRouterFragment(renamed) = %v", err)
	}
	if gotOriginal == gotRenamed {
		t.Fatal("openRouterFragment output did not change after Model.context_length was renamed")
	}
	if !strings.Contains(gotRenamed, "context_window:") {
		t.Fatalf("renamed fragment missing renamed field:\n%s", gotRenamed)
	}
}

func TestOllamaFragmentReflectsPinnedExampleRename(t *testing.T) {
	original := syntheticOllamaDocument("context_length")
	renamed := syntheticOllamaDocument("context_window")

	gotOriginal, err := ollamaFragment(original)
	if err != nil {
		t.Fatalf("ollamaFragment(original) = %v", err)
	}
	gotRenamed, err := ollamaFragment(renamed)
	if err != nil {
		t.Fatalf("ollamaFragment(renamed) = %v", err)
	}
	if gotOriginal == gotRenamed {
		t.Fatal("ollamaFragment output did not change after details.context_length was renamed")
	}
	if !strings.Contains(gotRenamed, `"context_window": 0`) {
		t.Fatalf("renamed fragment missing renamed field:\n%s", gotRenamed)
	}
	if strings.Contains(gotRenamed, "model-id") {
		t.Fatalf("derived fragment leaked concrete model name:\n%s", gotRenamed)
	}
}

func syntheticOpenRouterDocument(field string) string {
	return `openapi: 3.1.0
info:
  version: 1.0.0
servers:
  - url: https://api.invalid/api/v1
tags:
  - name: Models
paths:
  /models:
    get:
      operationId: getModels
      responses:
        '200':
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ListModelsResponse'
  /other:
    get: {}
components:
  schemas:
    ListModelsResponse:
      type: object
      properties:
        data:
          type: array
          items:
            $ref: '#/components/schemas/Model'
    Model:
      type: object
      properties:
        id:
          type: string
        ` + field + `:
          type: integer
        pricing:
          $ref: '#/components/schemas/PublicPricing'
        architecture:
          $ref: '#/components/schemas/ModelArchitecture'
    ModelArchitecture:
      type: object
      properties:
        input_modalities:
          type: array
    PublicPricing:
      type: object
      properties:
        prompt:
          type: string
    Other:
      type: object
`
}

func syntheticOllamaDocument(field string) string {
	return `# API

## List Local Models

GET /api/tags

Response

` + "```json" + `
{
  "models": [
    {
      "name": "model-id",
      "details": {
        "` + field + `": 2048,
        "parameter_size": "1B",
        "quantization_level": "Q4"
      },
      "capabilities": ["completion"]
    }
  ]
}
` + "```" + `

## List Running Models

GET /api/ps

Response

` + "```json" + `
{
  "models": [
    {
      "name": "model-id",
      "expires_at": "2026-01-01T00:00:00Z",
      "size_vram": 1
    }
  ]
}
` + "```" + `
`
}

// TestOpenRouterFragmentDropsExamples: schema examples are not schema; the
// upstream ones name concrete models, which a Epitrop fixture must not
// carry, so the extracted fragment keeps the shape and drops every example.
func TestOpenRouterFragmentDropsExamples(t *testing.T) {
	doc := strings.Replace(syntheticOpenRouterDocument("context_length"),
		"        id:\n          type: string\n",
		"        id:\n          type: string\n          example: vendor-x/secret-model\n", 1)
	doc = strings.Replace(doc,
		"    Model:\n      type: object\n",
		"    Model:\n      type: object\n      example:\n        id: vendor-x/secret-model\n        pricing:\n          prompt: '0.1'\n", 1)
	if !strings.Contains(doc, "vendor-x/secret-model") {
		t.Fatal("test setup: examples were not planted")
	}
	got, err := openRouterFragment(doc)
	if err != nil {
		t.Fatalf("openRouterFragment() = %v", err)
	}
	if strings.Contains(got, "vendor-x") || strings.Contains(got, "example") {
		t.Fatalf("fragment keeps an example:\n%s", got)
	}
	if !strings.Contains(got, "context_length:") || !strings.Contains(got, "type: string") {
		t.Fatalf("fragment lost schema while dropping examples:\n%s", got)
	}
}

// TestRecordDigest: refresh and check refuse a document whose digest differs
// from the manifest; repin, run after a pin moved, records the new digest
// and reports the change, so a Renovate pin bump can be refreshed.
func TestRecordDigest(t *testing.T) {
	doc := []byte("pinned upstream document")
	current := shaHex(doc)

	same := schemaSource{ID: "s", FullDocumentSHA256: current}
	if changed, err := recordDigest(&same, doc, false); err != nil || changed {
		t.Fatalf("matching digest: changed=%v err=%v, want unchanged and nil", changed, err)
	}

	stale := schemaSource{ID: "s", FullDocumentSHA256: "old"}
	if _, err := recordDigest(&stale, doc, false); err == nil {
		t.Fatal("refresh with a stale digest = nil error, want refusal")
	}
	if stale.FullDocumentSHA256 != "old" {
		t.Fatalf("refresh rewrote the digest to %q", stale.FullDocumentSHA256)
	}

	moved := schemaSource{ID: "s", FullDocumentSHA256: "old"}
	changed, err := recordDigest(&moved, doc, true)
	if err != nil || !changed || moved.FullDocumentSHA256 != current {
		t.Fatalf("repin: changed=%v err=%v digest=%q, want the new digest recorded", changed, err, moved.FullDocumentSHA256)
	}
}

const referencingDocument = `openapi: 3.1.0
info:
  title: Synthetic
  version: 1.0.0
servers:
  - url: https://api.invalid/v1
tags:
  - name: Chat
    description: Chat endpoints.
  - name: Models
    description: Model endpoints.
paths: {}
components:
  schemas:
    Top:
      $ref: "#/components/schemas/Middle"
    Middle:
      type: object
      properties:
        leaf:
          $ref: "#/components/schemas/Leaf"
        self:
          $ref: "#/components/schemas/Middle"
    Leaf:
      type: string
      example: dropped
`

func TestWithReferencedSchemasAddsTheTransitiveClosure(t *testing.T) {
	fragment := "openapi: 3.1.0\ncomponents:\n  schemas:\n    Top:\n      $ref: \"#/components/schemas/Middle\"\n"
	got, err := withReferencedSchemas(referencingDocument, fragment)
	if err != nil {
		t.Fatalf("withReferencedSchemas = %v", err)
	}
	if len(missingSchemas(got)) != 0 {
		t.Fatalf("references still unresolved: %v\n%s", missingSchemas(got), got)
	}
	for _, name := range []string{"    Middle:\n", "    Leaf:\n"} {
		if strings.Count(got, name) != 1 {
			t.Errorf("want %q exactly once:\n%s", name, got)
		}
	}
	if strings.Contains(got, "example:") {
		t.Errorf("added schema kept its example:\n%s", got)
	}
}

func TestWithReferencedSchemasRefusesAMissingSchema(t *testing.T) {
	fragment := "components:\n  schemas:\n    Top:\n      $ref: \"#/components/schemas/Absent\"\n"
	if _, err := withReferencedSchemas(referencingDocument, fragment); err == nil {
		t.Fatal("withReferencedSchemas accepted a reference the document does not define")
	}
}

func TestWithReferencedSchemasKeepsACompleteFragment(t *testing.T) {
	fragment := "components:\n  schemas:\n    Leaf:\n      type: string\n"
	got, err := withReferencedSchemas(referencingDocument, fragment)
	if err != nil || got != fragment {
		t.Fatalf("withReferencedSchemas = %q, %v; want the fragment unchanged", got, err)
	}
}

func TestDocumentHeadTakesOnlyTheModelsTag(t *testing.T) {
	got, err := documentHead(referencingDocument)
	if err != nil {
		t.Fatalf("documentHead = %v", err)
	}
	want := "openapi: 3.1.0\ninfo:\n  title: Synthetic\n  version: 1.0.0\nservers:\n  - url: https://api.invalid/v1\ntags:\n  - name: Models\n    description: Model endpoints.\n"
	if got != want {
		t.Fatalf("documentHead =\n%s\nwant\n%s", got, want)
	}
	if _, err := documentHead(strings.Replace(referencingDocument, "name: Models", "name: Other", 1)); err == nil {
		t.Fatal("documentHead accepted a document without a Models tag")
	}
}

func TestValidateSourceURL(t *testing.T) {
	valid := schemaSource{ID: "valid-https", URLTemplate: "https://example.invalid/{revision}/schema.json", PinnedRevision: "v1"}
	if err := validateSourceURL(valid); err != nil {
		t.Fatalf("validateSourceURL(https) = %v, want nil", err)
	}
	upper := schemaSource{ID: "upper-https", URLTemplate: "HTTPS://example.invalid/{revision}/schema.json", PinnedRevision: "v1"}
	if err := validateSourceURL(upper); err != nil {
		t.Fatalf("validateSourceURL(HTTPS) = %v, want nil", err)
	}
	requireSourceURLRefused(t, schemaSource{ID: "plain-http", URLTemplate: "http://example.invalid/{revision}/schema.json", PinnedRevision: "v1"}, "http://example.invalid/v1/schema.json")
	requireSourceURLRefused(t, schemaSource{ID: "no-scheme", URLTemplate: "//example.invalid/{revision}/schema.json", PinnedRevision: "v1"}, "//example.invalid/v1/schema.json")
	requireSourceURLRefused(t, schemaSource{ID: "empty-host", URLTemplate: "https:///path/{revision}/schema.json", PinnedRevision: "v1"}, "https:///path/v1/schema.json")
}

func requireSourceURLRefused(t *testing.T, source schemaSource, wantURL string) {
	t.Helper()
	err := validateSourceURL(source)
	if err == nil {
		t.Fatalf("validateSourceURL(%s) = nil, want refusal of %s", source.ID, wantURL)
	}
	if !strings.Contains(err.Error(), source.ID) || !strings.Contains(err.Error(), wantURL) {
		t.Fatalf("validateSourceURL(%s) = %v, want error naming %q and %q", source.ID, err, source.ID, wantURL)
	}
}

func TestReadManifestValidatesSourceURLs(t *testing.T) {
	if _, err := readManifest(filepath.Join("..", "..")); err != nil {
		t.Fatalf("readManifest(repo root) = %v, want nil", err)
	}
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, filepath.FromSlash(manifestRel))
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"sources":[{"id":"insecure-src","url_template":"http://example.invalid/{revision}/doc.json","pinned_revision":"r1","full_document_sha256":"00","spdx_license":"MIT","copyright_holder":"X","fragments":[]}]}`
	if err := os.WriteFile(manifestPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readManifest(tmp)
	if err == nil || !strings.Contains(err.Error(), "insecure-src") || !strings.Contains(err.Error(), "http://example.invalid/r1/doc.json") {
		t.Fatalf("readManifest(http source) = %v, want error naming source id and url", err)
	}
}

func TestCheckHTTPSRedirect(t *testing.T) {
	httpsReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.invalid/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkHTTPSRedirect(httpsReq, nil); err != nil {
		t.Fatalf("checkHTTPSRedirect(https) = %v, want nil", err)
	}
	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid/downgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkHTTPSRedirect(httpReq, nil); err == nil {
		t.Fatal("checkHTTPSRedirect(http) = nil, want error")
	}
	if err := checkHTTPSRedirect(httpsReq, make([]*http.Request, 9)); err != nil {
		t.Fatalf("checkHTTPSRedirect(9 hops) = %v, want nil: the cap is Go's default of 10", err)
	}
	if err := checkHTTPSRedirect(httpsReq, make([]*http.Request, 10)); err == nil {
		t.Fatal("checkHTTPSRedirect(10 hops) = nil, want error")
	}
}
