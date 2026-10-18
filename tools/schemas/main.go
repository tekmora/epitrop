//go:build schemasrefresh

// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tekmora/epitrop/internal/sources/httpfetch"
)

const (
	manifestRel    = "internal/sources/upstream-schemas.json"
	extractionDate = "2026-10-09"
	fetchTimeout   = 60 * time.Second
	maxFetchBytes  = 16 << 20
	maxJSONTokens  = 100000
)

type schemaManifest struct {
	Sources []schemaSource `json:"sources"`
}

type schemaSource struct {
	ID                 string           `json:"id"`
	URLTemplate        string           `json:"url_template"`
	PinnedRevision     string           `json:"pinned_revision"`
	PinDescription     string           `json:"pin_description,omitempty"`
	ExtractionDate     string           `json:"extraction_date,omitempty"`
	FullDocumentSHA256 string           `json:"full_document_sha256"`
	SPDXLicense        string           `json:"spdx_license"`
	CopyrightHolder    string           `json:"copyright_holder"`
	Fragments          []schemaFragment `json:"fragments"`
}

type schemaFragment struct {
	Path           string `json:"path"`
	ProvenancePath string `json:"provenance_path,omitempty"`
	BodySHA256     string `json:"body_sha256"`
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	mode := flag.String("mode", "refresh", "refresh, repin or check")
	root := flag.String("root", ".", "repository root")
	out := flag.String("out", ".", "output root for refresh")
	flag.Parse()

	manifest, err := readManifest(*root)
	if err != nil {
		return err
	}
	switch *mode {
	case "refresh":
		return refresh(ctx, manifest, *out, false)
	case "repin":
		return refresh(ctx, manifest, *out, true)
	case "check":
		return check(ctx, manifest, *root)
	default:
		return fmt.Errorf("unknown mode %q", *mode)
	}
}

func readManifest(root string) (schemaManifest, error) {
	body, err := os.ReadFile(filepath.Join(root, manifestRel))
	if err != nil {
		return schemaManifest{}, fmt.Errorf("read schema manifest: %w", err)
	}
	var manifest schemaManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return schemaManifest{}, fmt.Errorf("decode schema manifest: %w", err)
	}
	return manifest, nil
}

func refresh(ctx context.Context, manifest schemaManifest, outRoot string, repin bool) error {
	for i := 0; i < len(manifest.Sources); i++ {
		full, err := fetchPinned(ctx, manifest.Sources[i])
		if err != nil {
			return err
		}
		old := manifest.Sources[i].FullDocumentSHA256
		changed, err := recordDigest(&manifest.Sources[i], full, repin)
		if err != nil {
			return err
		}
		if changed {
			fmt.Printf("%s: full_document_sha256 %s -> %s at %s\n", manifest.Sources[i].ID, old, manifest.Sources[i].FullDocumentSHA256, manifest.Sources[i].PinnedRevision)
		}
		source := manifest.Sources[i]
		fragments, err := generateFragments(source, full)
		if err != nil {
			return err
		}
		if err := writeFragments(outRoot, source, fragments); err != nil {
			return err
		}
		if err := updateFragmentDigests(&manifest.Sources[i], fragments); err != nil {
			return err
		}
	}
	return writeManifest(outRoot, manifest)
}

func check(ctx context.Context, manifest schemaManifest, root string) error {
	tmp, err := os.MkdirTemp("", "epitrop-schema-check-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	if err := refresh(ctx, manifest, tmp, false); err != nil {
		return err
	}
	return compareFragments(manifest, root, tmp)
}

func fetchPinned(ctx context.Context, source schemaSource) ([]byte, error) {
	body, err := httpfetch.Get(ctx, sourceURL(source), fetchTimeout, maxFetchBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: fetch: %w", source.ID, err)
	}
	return body, nil
}

func sourceURL(source schemaSource) string {
	return strings.ReplaceAll(source.URLTemplate, "{revision}", source.PinnedRevision)
}

func generateFragments(source schemaSource, full []byte) (map[string][]byte, error) {
	switch source.ID {
	case "openai-openapi":
		body, err := openAIModelsFragment(string(full))
		if err != nil {
			return nil, err
		}
		return oneFragment(source, body)
	case "litellm-price-map":
		return liteLLMFragment(source, full)
	case "openrouter-models":
		body, err := openRouterFragment(string(full))
		if err != nil {
			return nil, err
		}
		return oneFragment(source, body)
	case "ollama-api":
		body, err := ollamaFragment(string(full))
		if err != nil {
			return nil, err
		}
		return oneFragment(source, body)
	case "codex-rate-limit-status-payload",
		"codex-rate-limit-status-details",
		"codex-rate-limit-window-snapshot":
		return oneFragment(source, string(full))
	case "litellm-key-info":
		body, err := liteLLMKeyInfoFragment(string(full))
		if err != nil {
			return nil, err
		}
		return oneFragment(source, body)
	default:
		return nil, fmt.Errorf("unknown schema source %q", source.ID)
	}
}

func oneFragment(source schemaSource, body string) (map[string][]byte, error) {
	if len(source.Fragments) != 1 {
		return nil, fmt.Errorf("%s: got %d fragments, want 1", source.ID, len(source.Fragments))
	}
	fragment := source.Fragments[0]
	result := map[string][]byte{}
	if fragment.ProvenancePath == "" {
		bodyWithHeader, err := addCommentHeader(fragment.Path, source, body)
		if err != nil {
			return nil, err
		}
		result[fragment.Path] = bodyWithHeader
	} else {
		provenance, err := jsonProvenance(source)
		if err != nil {
			return nil, err
		}
		result[fragment.Path] = []byte(body)
		result[fragment.ProvenancePath] = provenance
	}
	return result, nil
}

// sourceExtractionDate is the date a source's fragment was first extracted;
// sources added after the first extraction name their own.
func sourceExtractionDate(source schemaSource) string {
	if source.ExtractionDate != "" {
		return source.ExtractionDate
	}
	return extractionDate
}

// liteLLMKeyInfoFragment takes the docstring of LiteLLM's GET /key/info
// handler, which documents every field of the response's info object.
func liteLLMKeyInfoFragment(full string) (string, error) {
	const route = `@router.get("/key/info"`
	start := strings.Index(full, route)
	if start < 0 {
		return "", fmt.Errorf("litellm-key-info: missing marker %q", route)
	}
	rest := full[start:]
	open := strings.Index(rest, `"""`)
	if open < 0 {
		return "", errors.New("litellm-key-info: the handler has no docstring")
	}
	rest = rest[open+3:]
	end := strings.Index(rest, `"""`)
	if end < 0 {
		return "", errors.New("litellm-key-info: unterminated docstring")
	}
	// The curl examples carry placeholder keys a secret scanner reports;
	// the contract is the parameters and the returned fields before them.
	examples := strings.Index(rest[:end], "Example Curl")
	if examples < 0 {
		return "", errors.New("litellm-key-info: the docstring has no Example Curl section to cut at")
	}
	return joinSections(
		"# LiteLLM GET /key/info",
		"",
		"Docstring of the handler, verbatim up to its curl examples:",
		"",
		"```text",
		strings.TrimRight(rest[:examples], " \n"),
		"```",
	), nil
}

func liteLLMFragment(source schemaSource, full []byte) (map[string][]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(full, &raw); err != nil {
		return nil, fmt.Errorf("%s: decode price map: %w", source.ID, err)
	}
	spec, ok := raw["sample_spec"]
	if !ok {
		return nil, errors.New("litellm-price-map: sample_spec missing")
	}
	body, err := formatJSONObject(map[string]json.RawMessage{"sample_spec": spec})
	if err != nil {
		return nil, err
	}
	return oneFragment(source, string(body))
}

func writeFragments(outRoot string, source schemaSource, fragments map[string][]byte) error {
	for path, body := range fragments {
		fullPath := filepath.Join(outRoot, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return fmt.Errorf("%s: create dir for %s: %w", source.ID, path, err)
		}
		if err := os.WriteFile(fullPath, body, 0o644); err != nil {
			return fmt.Errorf("%s: write %s: %w", source.ID, path, err)
		}
	}
	return nil
}

func updateFragmentDigests(source *schemaSource, fragments map[string][]byte) error {
	for i := 0; i < len(source.Fragments); i++ {
		fragment := source.Fragments[i]
		body, ok := fragments[fragment.Path]
		if !ok {
			return fmt.Errorf("%s: generated %s missing", source.ID, fragment.Path)
		}
		hashBody, err := generatedBodyForHash(fragment, body)
		if err != nil {
			return err
		}
		source.Fragments[i].BodySHA256 = shaHex(hashBody)
	}
	return nil
}

func generatedBodyForHash(fragment schemaFragment, body []byte) ([]byte, error) {
	if fragment.ProvenancePath != "" {
		return body, nil
	}
	_, rest, ok := bytes.Cut(body, []byte("\n\n"))
	if !ok {
		return nil, fmt.Errorf("%s: generated provenance header missing separator", fragment.Path)
	}
	return rest, nil
}

func writeManifest(outRoot string, manifest schemaManifest) error {
	body, err := formatJSONObject(manifest)
	if err != nil {
		return err
	}
	path := filepath.Join(outRoot, manifestRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", manifestRel, err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", manifestRel, err)
	}
	return nil
}

func compareFragments(manifest schemaManifest, root, tmp string) error {
	for i := 0; i < len(manifest.Sources); i++ {
		for j := 0; j < len(manifest.Sources[i].Fragments); j++ {
			fragment := manifest.Sources[i].Fragments[j]
			if err := compareOne(root, tmp, fragment.Path); err != nil {
				return err
			}
			if fragment.ProvenancePath != "" {
				if err := compareOne(root, tmp, fragment.ProvenancePath); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func compareOne(root, tmp, rel string) error {
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("read committed %s: %w", rel, err)
	}
	want, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("read refreshed %s: %w", rel, err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s differs from pinned refresh", rel)
	}
	return nil
}

func openAIModelsFragment(full string) (string, error) {
	path, err := extractBlock(full, "  /models:", "\n  /")
	if err != nil {
		return "", err
	}
	list, err := extractBlock(full, "\n    ListModelsResponse:\n", "\n    List")
	if err != nil {
		return "", err
	}
	model, err := extractBlock(full, "\n    Model:\n", "\n    ModelIds:")
	if err != nil {
		return "", err
	}
	head, err := documentHead(full)
	if err != nil {
		return "", err
	}
	return withReferencedSchemas(full, joinSections(
		head,
		"paths:",
		strings.TrimRight(beforeMarker(path, "\n      x-oaiMeta:"), "\n"),
		"components:",
		"  schemas:",
		strings.TrimRight(strings.TrimLeft(list, "\n"), "\n"),
		strings.TrimRight(strings.TrimLeft(beforeMarker(model, "\n      x-oaiMeta:"), "\n"), "\n"),
	))
}

func openRouterFragment(full string) (string, error) {
	doc := yamlDocument(full)
	path, err := extractYAMLBlock(doc, "  /models:")
	if err != nil {
		return "", err
	}
	if !strings.Contains(path, "operationId: getModels") {
		return "", errors.New("openrouter-models: /models getModels operation missing")
	}
	response, err := extractFirstSchemaBlock(doc, []string{"ListModelsResponse", "ModelsListResponse"})
	if err != nil {
		return "", err
	}
	data, err := optionalSchemaBlock(doc, "ModelsListResponseData")
	if err != nil {
		return "", err
	}
	model, err := extractSchemaBlock(doc, "Model")
	if err != nil {
		return "", err
	}
	arch, err := extractSchemaBlock(doc, "ModelArchitecture")
	if err != nil {
		return "", err
	}
	pricing, err := extractSchemaBlock(doc, "PublicPricing")
	if err != nil {
		return "", err
	}
	head, err := documentHead(doc)
	if err != nil {
		return "", err
	}
	return withReferencedSchemas(doc, joinSections(
		head,
		"paths:",
		strings.TrimRight(path, "\n"),
		"components:",
		"  schemas:",
		strings.TrimRight(response, "\n"),
		strings.TrimRight(data, "\n"),
		strings.TrimRight(model, "\n"),
		strings.TrimRight(arch, "\n"),
		strings.TrimRight(pricing, "\n"),
	))
}

// documentHead returns the openapi line with the document's info and servers blocks and its
// Models tag, so that an extract is a complete OpenAPI document a linter can read.
func documentHead(doc string) (string, error) {
	info, err := extractYAMLBlock(doc, "info:")
	if err != nil {
		return "", err
	}
	servers, err := extractYAMLBlock(doc, "servers:")
	if err != nil {
		return "", err
	}
	tag, err := tagItem(doc, "Models")
	if err != nil {
		return "", err
	}
	return joinSections("openapi: 3.1.0", info, servers, "tags:", tag), nil
}

// tagItem returns the item of the document's top-level tags list that carries the name.
func tagItem(doc, name string) (string, error) {
	tags, err := extractYAMLBlock(doc, "tags:")
	if err != nil {
		return "", err
	}
	lines := strings.SplitAfter(tags, "\n")
	starts := make([]int, 0, len(lines))
	for i := 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "  - ") {
			starts = append(starts, i)
		}
	}
	starts = append(starts, len(lines))
	nameLine := regexp.MustCompile(`(?m)^  (- |  )name: ` + regexp.QuoteMeta(name) + `$`)
	for k := 0; k+1 < len(starts); k++ {
		item := strings.Join(lines[starts[k]:starts[k+1]], "")
		if nameLine.MatchString(item) {
			return item, nil
		}
	}
	return "", fmt.Errorf("missing tag %q", name)
}

// maxSchemaRounds bounds the rounds that add referenced schemas; the deepest chain in the
// pinned documents takes three.
const maxSchemaRounds = 32

var (
	schemaRef      = regexp.MustCompile(`#/components/schemas/([A-Za-z0-9_.-]+)`)
	schemaDefining = regexp.MustCompile(`(?m)^    ([A-Za-z0-9_.-]+):$`)
)

// withReferencedSchemas appends, from the upstream document, every schema the fragment
// references but does not define, round by round, until each reference resolves.
func withReferencedSchemas(doc, fragment string) (string, error) {
	for round := 0; round < maxSchemaRounds; round++ {
		missing := missingSchemas(fragment)
		if len(missing) == 0 {
			return fragment, nil
		}
		blocks := make([]string, 0, len(missing)+1)
		blocks = append(blocks, fragment)
		for i := 0; i < len(missing); i++ {
			block, err := extractSchemaBlock(doc, missing[i])
			if err != nil {
				return "", err
			}
			blocks = append(blocks, strings.TrimRight(beforeMarker(block, "\n      x-oaiMeta:"), "\n"))
		}
		fragment = joinSections(blocks...)
	}
	return "", fmt.Errorf("schema references still unresolved after %d rounds", maxSchemaRounds)
}

// missingSchemas returns, sorted, the schema names the fragment references and its
// components.schemas section does not define.
func missingSchemas(fragment string) []string {
	defined := map[string]bool{}
	if at := strings.Index(fragment, "\n  schemas:\n"); at >= 0 {
		names := schemaDefining.FindAllStringSubmatch(fragment[at:], -1)
		for i := 0; i < len(names); i++ {
			defined[names[i][1]] = true
		}
	}
	var missing []string
	refs := schemaRef.FindAllStringSubmatch(fragment, -1)
	for i := 0; i < len(refs); i++ {
		if !defined[refs[i][1]] {
			defined[refs[i][1]] = true
			missing = append(missing, refs[i][1])
		}
	}
	sort.Strings(missing)
	return missing
}

func yamlDocument(full string) string {
	start := strings.Index(full, "````yaml")
	if start < 0 {
		return full
	}
	bodyStart := strings.IndexByte(full[start:], '\n')
	if bodyStart < 0 {
		return full
	}
	rest := full[start+bodyStart+1:]
	end := strings.Index(rest, "````")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func extractFirstSchemaBlock(full string, names []string) (string, error) {
	var lastErr error
	for i := 0; i < len(names); i++ {
		block, err := extractSchemaBlock(full, names[i])
		if err == nil {
			return block, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func optionalSchemaBlock(full, name string) (string, error) {
	block, err := extractSchemaBlock(full, name)
	if err != nil && strings.Contains(err.Error(), "missing YAML block") {
		return "", nil
	}
	return block, err
}

func extractSchemaBlock(full, name string) (string, error) {
	return extractYAMLBlock(full, "    "+name+":")
}

func extractYAMLBlock(full, marker string) (string, error) {
	lines := strings.SplitAfter(full, "\n")
	start := -1
	for i := 0; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r\n") == marker {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("missing YAML block %q", marker)
	}
	indent := leadingSpaces(marker)
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if leadingSpaces(lines[i]) <= indent {
			end = i
			break
		}
	}
	return stripExampleKeys(strings.Join(lines[start:end], "")), nil
}

// stripExampleKeys drops every example and examples key, with the lines
// nested under it, from a YAML block. Examples document a schema without
// being part of it, and upstream ones name concrete models.
func stripExampleKeys(block string) string {
	lines := strings.SplitAfter(block, "\n")
	kept := make([]string, 0, len(lines))
	skipBelow := -1
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		indent := leadingSpaces(lines[i])
		if skipBelow >= 0 {
			if trimmed == "" || indent > skipBelow {
				continue
			}
			skipBelow = -1
		}
		if strings.HasPrefix(trimmed, "example:") || strings.HasPrefix(trimmed, "examples:") {
			skipBelow = indent
			continue
		}
		kept = append(kept, lines[i])
	}
	return strings.Join(kept, "")
}

func ollamaFragment(full string) (string, error) {
	tags, err := ollamaExampleShape(full, "## List Local Models")
	if err != nil {
		return "", err
	}
	ps, err := ollamaExampleShape(full, "## List Running Models")
	if err != nil {
		return "", err
	}
	return joinSections(
		"# Ollama API response fragments",
		"",
		"## GET /api/tags",
		"",
		"```json",
		tags,
		"```",
		"",
		"## GET /api/ps",
		"",
		"```json",
		ps,
		"```",
	), nil
}

func ollamaExampleShape(full, heading string) (string, error) {
	section, err := markdownSection(full, heading)
	if err != nil {
		return "", err
	}
	raw, err := firstJSONFence(section)
	if err != nil {
		return "", fmt.Errorf("%s: %w", heading, err)
	}
	shape, err := deriveJSONShape(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", heading, err)
	}
	return shape, nil
}

func markdownSection(full, heading string) (string, error) {
	start := strings.Index(full, heading)
	if start < 0 {
		return "", fmt.Errorf("missing heading %q", heading)
	}
	rest := full[start+len(heading):]
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		return rest, nil
	}
	return rest[:end], nil
}

func firstJSONFence(section string) (string, error) {
	start := strings.Index(section, "```json\n")
	if start < 0 {
		return "", errors.New("missing json response example")
	}
	rest := section[start+len("```json\n"):]
	end := strings.Index(rest, "\n```")
	if end < 0 {
		return "", errors.New("unterminated json response example")
	}
	return rest[:end], nil
}

func extractBlock(full, start, end string) (string, error) {
	startAt := strings.Index(full, start)
	if startAt < 0 {
		return "", fmt.Errorf("missing marker %q", start)
	}
	rest := full[startAt:]
	endAt := strings.Index(rest[len(start):], end)
	if endAt < 0 {
		return rest, nil
	}
	return rest[:len(start)+endAt], nil
}

func leadingSpaces(s string) int {
	count := 0
	for count < len(s) && s[count] == ' ' {
		count++
	}
	return count
}

func beforeMarker(body, marker string) string {
	at := strings.Index(body, marker)
	if at < 0 {
		return body
	}
	return body[:at]
}

func requireMarkers(id, full string, markers []string) error {
	for i := 0; i < len(markers); i++ {
		if !strings.Contains(full, markers[i]) {
			return fmt.Errorf("%s: missing marker %q", id, markers[i])
		}
	}
	return nil
}

func addCommentHeader(path string, source schemaSource, body string) ([]byte, error) {
	header := provenanceLines(source)
	switch filepath.Ext(path) {
	case ".yaml", ".yml":
		return []byte(commentHeader("# ", header) + bodyWithNewline(body)), nil
	case ".md":
		return []byte(markdownHeader(header) + bodyWithNewline(body)), nil
	case ".rs":
		return []byte(commentHeader("// ", header) + bodyWithNewline(body)), nil
	default:
		return nil, fmt.Errorf("unsupported provenance header for %s", path)
	}
}

func provenanceLines(source schemaSource) []string {
	return []string{
		"Provenance:",
		"Source URL: " + sourceURL(source),
		"Pinned revision: " + source.PinnedRevision,
		"Full document sha256: " + source.FullDocumentSHA256,
		"Extraction date: " + sourceExtractionDate(source),
		"SPDX licence: " + source.SPDXLicense,
		"Copyright holder: " + source.CopyrightHolder,
		"Changes: provenance header added by Epitrop; " + extractionChange(source.ID),
	}
}

func extractionChange(id string) string {
	switch id {
	case "openai-openapi":
		return "extracted info, servers, the Models tag, /models path and ListModelsResponse/Model schema blocks, with every schema they reference"
	case "litellm-price-map":
		return "extracted the sample_spec entry"
	case "openrouter-models":
		return "extracted info, servers, the Models tag, getModels path and model schema blocks, with every schema they reference"
	case "ollama-api":
		return "derived /api/tags and /api/ps response shapes from examples"
	case "codex-rate-limit-status-payload",
		"codex-rate-limit-status-details",
		"codex-rate-limit-window-snapshot":
		return "extracted the pinned Codex source file verbatim"
	case "litellm-key-info":
		return "extracted the /key/info handler docstring verbatim"
	default:
		return "extracted the pinned upstream fragment"
	}
}

func commentHeader(prefix string, lines []string) string {
	var b strings.Builder
	for i := 0; i < len(lines); i++ {
		b.WriteString(prefix)
		b.WriteString(lines[i])
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

func markdownHeader(lines []string) string {
	var b strings.Builder
	b.WriteString("<!--\n")
	for i := 0; i < len(lines); i++ {
		b.WriteString(lines[i])
		b.WriteByte('\n')
	}
	b.WriteString("-->\n\n")
	return b.String()
}

func jsonProvenance(source schemaSource) ([]byte, error) {
	body, err := formatJSONObject(map[string]string{
		"source_url":             sourceURL(source),
		"pinned_revision":        source.PinnedRevision,
		"full_document_sha256":   source.FullDocumentSHA256,
		"extraction_date":        sourceExtractionDate(source),
		"spdx_license":           source.SPDXLicense,
		"copyright_holder":       source.CopyrightHolder,
		"source_pin_description": source.PinDescription,
		"changes":                "provenance header added by Epitrop; " + extractionChange(source.ID),
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

type orderedKind byte

const (
	orderedObject orderedKind = 'o'
	orderedArray  orderedKind = 'a'
	orderedScalar orderedKind = 's'
)

type orderedValue struct {
	kind   orderedKind
	fields []orderedField
	elems  []orderedValue
	scalar any
}

type orderedField struct {
	key   string
	value orderedValue
}

type jsonFrame struct {
	value   orderedValue
	key     string
	wantKey bool
}

func deriveJSONShape(raw string) (string, error) {
	root, err := parseOrderedJSON(raw)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := renderOrderedJSON(&b, root); err != nil {
		return "", err
	}
	b.WriteByte('\n')
	return b.String(), nil
}

func parseOrderedJSON(raw string) (orderedValue, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var root orderedValue
	rootSet := false
	stack := []jsonFrame{}
	for step := 0; step < maxJSONTokens; step++ {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return finishOrderedParse(root, rootSet, stack)
		}
		if err != nil {
			return orderedValue{}, fmt.Errorf("decode json example: %w", err)
		}
		var consumeErr error
		root, rootSet, consumeErr = consumeJSONToken(tok, &stack, root, rootSet)
		if consumeErr != nil {
			return orderedValue{}, consumeErr
		}
	}
	return orderedValue{}, fmt.Errorf("json example exceeds %d tokens", maxJSONTokens)
}

func consumeJSONToken(tok json.Token, stack *[]jsonFrame, root orderedValue, rootSet bool) (orderedValue, bool, error) {
	value, ok := orderedValueFromToken(tok, stack)
	if !ok {
		return root, rootSet, nil
	}
	return attachOrderedValue(*stack, root, rootSet, value)
}

func orderedValueFromToken(tok json.Token, stack *[]jsonFrame) (orderedValue, bool) {
	switch v := tok.(type) {
	case json.Delim:
		return handleJSONDelim(v, stack)
	case string:
		if consumeObjectKey(stack, v) {
			return orderedValue{}, false
		}
		return orderedValue{kind: orderedScalar, scalar: "string"}, true
	case json.Number:
		return orderedValue{kind: orderedScalar, scalar: 0}, true
	case bool:
		return orderedValue{kind: orderedScalar, scalar: false}, true
	case nil:
		return orderedValue{kind: orderedScalar, scalar: nil}, true
	default:
		return orderedValue{}, false
	}
}

func handleJSONDelim(delim json.Delim, stack *[]jsonFrame) (orderedValue, bool) {
	switch delim {
	case '{':
		*stack = append(*stack, jsonFrame{value: orderedValue{kind: orderedObject}, wantKey: true})
	case '[':
		*stack = append(*stack, jsonFrame{value: orderedValue{kind: orderedArray}})
	case '}', ']':
		if len(*stack) == 0 {
			return orderedValue{}, false
		}
		last := len(*stack) - 1
		closed := (*stack)[last].value
		*stack = (*stack)[:last]
		return closed, true
	}
	return orderedValue{}, false
}

func consumeObjectKey(stack *[]jsonFrame, key string) bool {
	if len(*stack) == 0 {
		return false
	}
	top := &(*stack)[len(*stack)-1]
	if top.value.kind != orderedObject || !top.wantKey {
		return false
	}
	top.key = key
	top.wantKey = false
	return true
}

func attachOrderedValue(stack []jsonFrame, root orderedValue, rootSet bool, value orderedValue) (orderedValue, bool, error) {
	if len(stack) == 0 {
		if rootSet {
			return root, true, errors.New("json example has multiple roots")
		}
		return value, true, nil
	}
	parent := &stack[len(stack)-1]
	if parent.value.kind == orderedArray {
		parent.value.elems = append(parent.value.elems, value)
		return root, rootSet, nil
	}
	if parent.wantKey {
		return root, rootSet, errors.New("json object value missing key")
	}
	parent.value.fields = append(parent.value.fields, orderedField{key: parent.key, value: value})
	parent.key = ""
	parent.wantKey = true
	return root, rootSet, nil
}

func finishOrderedParse(root orderedValue, rootSet bool, stack []jsonFrame) (orderedValue, error) {
	if len(stack) != 0 {
		return orderedValue{}, errors.New("json example ended inside a container")
	}
	if !rootSet {
		return orderedValue{}, errors.New("json example was empty")
	}
	return root, nil
}

type renderFrame struct {
	value orderedValue
	depth int
	index int
	open  bool
}

func renderOrderedJSON(b *strings.Builder, root orderedValue) error {
	stack := []renderFrame{{value: root}}
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		switch top.value.kind {
		case orderedScalar:
			writeJSONScalar(b, top.value.scalar)
			stack = stack[:len(stack)-1]
		case orderedObject:
			if renderObjectStep(b, &stack) {
				continue
			}
		case orderedArray:
			if renderArrayStep(b, &stack) {
				continue
			}
		default:
			return fmt.Errorf("unknown ordered json kind %q", top.value.kind)
		}
	}
	return nil
}

func renderObjectStep(b *strings.Builder, stack *[]renderFrame) bool {
	top := &(*stack)[len(*stack)-1]
	if len(top.value.fields) == 0 {
		b.WriteString("{}")
		*stack = (*stack)[:len(*stack)-1]
		return true
	}
	if !top.open {
		b.WriteString("{")
		top.open = true
	}
	if top.index >= len(top.value.fields) {
		b.WriteByte('\n')
		writeIndent(b, top.depth)
		b.WriteByte('}')
		*stack = (*stack)[:len(*stack)-1]
		return true
	}
	field := top.value.fields[top.index]
	writeElementPrefix(b, top.depth, top.index)
	b.WriteString(strconv.Quote(field.key))
	b.WriteString(": ")
	top.index++
	*stack = append(*stack, renderFrame{value: field.value, depth: top.depth + 1})
	return true
}

func renderArrayStep(b *strings.Builder, stack *[]renderFrame) bool {
	top := &(*stack)[len(*stack)-1]
	limit := len(top.value.elems)
	if limit > 1 {
		limit = 1
	}
	if limit == 0 {
		b.WriteString("[]")
		*stack = (*stack)[:len(*stack)-1]
		return true
	}
	if !top.open {
		b.WriteString("[")
		top.open = true
	}
	if top.index >= limit {
		b.WriteByte('\n')
		writeIndent(b, top.depth)
		b.WriteByte(']')
		*stack = (*stack)[:len(*stack)-1]
		return true
	}
	writeElementPrefix(b, top.depth, top.index)
	child := top.value.elems[top.index]
	top.index++
	*stack = append(*stack, renderFrame{value: child, depth: top.depth + 1})
	return true
}

func writeElementPrefix(b *strings.Builder, depth, index int) {
	if index > 0 {
		b.WriteByte(',')
	}
	b.WriteByte('\n')
	writeIndent(b, depth+1)
}

func writeIndent(b *strings.Builder, depth int) {
	for i := 0; i < depth; i++ {
		b.WriteString("  ")
	}
}

func writeJSONScalar(b *strings.Builder, scalar any) {
	switch v := scalar.(type) {
	case string:
		b.WriteString(strconv.Quote(v))
	case int:
		b.WriteString(strconv.Itoa(v))
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	default:
		b.WriteString("null")
	}
}

func formatJSONObject(v any) ([]byte, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("format json: %w", err)
	}
	return append(body, '\n'), nil
}

func bodyWithNewline(body string) string {
	return strings.TrimRight(body, "\n") + "\n"
}

func joinSections(sections ...string) string {
	var b strings.Builder
	for i := 0; i < len(sections); i++ {
		b.WriteString(strings.TrimRight(sections[i], "\n"))
		b.WriteByte('\n')
	}
	return b.String()
}

func shaHex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// recordDigest compares the fetched document with the digest the manifest
// records. refresh and check refuse a mismatch. repin, run after a pin moved,
// records the new digest instead and reports that it changed, so the change
// shows up in the manifest diff a reviewer reads.
func recordDigest(source *schemaSource, full []byte, repin bool) (bool, error) {
	got := shaHex(full)
	if got == source.FullDocumentSHA256 {
		return false, nil
	}
	if !repin {
		return false, fmt.Errorf("%s: sha256 %s, want %s (after a pin moved, run make schemas-repin)", source.ID, got, source.FullDocumentSHA256)
	}
	source.FullDocumentSHA256 = got
	return true, nil
}
