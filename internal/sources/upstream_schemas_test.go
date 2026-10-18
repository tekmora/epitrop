package sources

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifestPath = "upstream-schemas.json"

type testManifest struct {
	Sources []testSource `json:"sources"`
}

type testSource struct {
	ID                 string         `json:"id"`
	URLTemplate        string         `json:"url_template"`
	PinnedRevision     string         `json:"pinned_revision"`
	ExtractionDate     string         `json:"extraction_date"`
	FullDocumentSHA256 string         `json:"full_document_sha256"`
	SPDXLicense        string         `json:"spdx_license"`
	CopyrightHolder    string         `json:"copyright_holder"`
	Fragments          []testFragment `json:"fragments"`
}

type testFragment struct {
	Path           string `json:"path"`
	ProvenancePath string `json:"provenance_path"`
	BodySHA256     string `json:"body_sha256"`
}

func TestUpstreamSchemaFragmentsHaveProvenanceAndBodyHashes(t *testing.T) {
	manifest := readTestManifest(t)
	for i := 0; i < len(manifest.Sources); i++ {
		source := manifest.Sources[i]
		for j := 0; j < len(source.Fragments); j++ {
			fragment := source.Fragments[j]
			body := readFragmentBody(t, source, fragment)
			if got := shaHex(body); got != fragment.BodySHA256 {
				t.Fatalf("%s body sha256 = %s, want %s", fragment.Path, got, fragment.BodySHA256)
			}
		}
	}
}

func readTestManifest(t *testing.T) testManifest {
	t.Helper()
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	var manifest testManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode %s: %v", manifestPath, err)
	}
	return manifest
}

func readFragmentBody(t *testing.T, source testSource, fragment testFragment) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(fragment.Path)))
	if err != nil {
		t.Fatalf("read %s: %v", fragment.Path, err)
	}
	if fragment.ProvenancePath != "" {
		checkJSONProvenance(t, source, fragment.ProvenancePath)
		return body
	}
	checkCommentProvenance(t, source, body, fragment.Path)
	return stripCommentHeader(t, body, fragment.Path)
}

func checkJSONProvenance(t *testing.T, source testSource, path string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, want := range provenanceValues(source) {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("%s provenance missing %q", path, want)
		}
	}
}

func checkCommentProvenance(t *testing.T, source testSource, body []byte, path string) {
	t.Helper()
	header, _, ok := bytes.Cut(body, []byte("\n\n"))
	if !ok {
		t.Fatalf("%s missing provenance header separator", path)
	}
	for _, want := range provenanceValues(source) {
		if !bytes.Contains(header, []byte(want)) {
			t.Fatalf("%s provenance missing %q", path, want)
		}
	}
}

func provenanceValues(source testSource) []string {
	return []string{
		strings.ReplaceAll(source.URLTemplate, "{revision}", source.PinnedRevision),
		source.PinnedRevision,
		source.FullDocumentSHA256,
		extractionDate(source),
		source.SPDXLicense,
		source.CopyrightHolder,
	}
}

func stripCommentHeader(t *testing.T, body []byte, path string) []byte {
	t.Helper()
	_, rest, ok := bytes.Cut(body, []byte("\n\n"))
	if !ok {
		t.Fatalf("%s missing provenance header separator", path)
	}
	return rest
}

func shaHex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// extractionDate is the date a source's fragment was first extracted; the
// first sources share the date the manifest was made.
func extractionDate(source testSource) string {
	if source.ExtractionDate != "" {
		return source.ExtractionDate
	}
	return "2026-10-09"
}
