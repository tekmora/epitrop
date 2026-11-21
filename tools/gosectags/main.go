// Command gosectags discovers build tags and verifies two-pass scan coverage.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	commandTimeout = 30 * time.Second
	maxFiles       = 100000
	maxTags        = 8
	maxNodes       = 1024
	// go help buildconstraint lists these fixed toolchain-supplied tags.
	fixedTags = "unix cgo gc gccgo"
)

var releaseTag = regexp.MustCompile(`^go1\.[0-9]+$`)

func gitCommand(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", args...).Output() // #nosec G204 -- fixed git executable; arguments are supplied by discovery
	if err != nil {
		return nil, fmt.Errorf("git: %w", err)
	}
	return out, nil
}

func goCommand(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", args...).Output() // #nosec G204 -- fixed go executable; arguments are supplied by discovery
	if err != nil {
		return nil, fmt.Errorf("go: %w", err)
	}
	return out, nil
}

// checkInheritedTags asks the Go command for tags in effect because it parses
// GOFLAGS itself, including quoting.
func checkInheritedTags() error {
	out, err := goCommand("list", "-e", "-find", "-f", "{{join context.BuildTags \",\"}}", "unsafe")
	if err != nil {
		return fmt.Errorf("read build tags from GOFLAGS: %w", err)
	}
	if tags := strings.TrimSpace(string(out)); tags != "" {
		return fmt.Errorf("GOFLAGS sets build tags %s", tags)
	}
	return nil
}

func moduleIgnores(root string) ([]string, error) {
	out, err := goCommand("-C", root, "mod", "edit", "-json")
	if err != nil {
		return nil, fmt.Errorf("%s: read go.mod: %w", root, err)
	}
	var mod struct{ Ignore []struct{ Path string } }
	if err := json.Unmarshal(out, &mod); err != nil {
		return nil, fmt.Errorf("%s: parse go.mod: %w", root, err)
	}
	patterns := make([]string, 0, len(mod.Ignore))
	for _, entry := range mod.Ignore {
		patterns = append(patterns, entry.Path)
	}
	return patterns, nil
}

func ignoredDir(dir string, patterns []string) bool {
	dir = "/" + dir + "/"
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, "./") {
			if strings.HasPrefix(dir, "/"+strings.TrimPrefix(pattern, "./")+"/") {
				return true
			}
		} else if strings.Contains(dir, "/"+pattern+"/") {
			return true
		}
	}
	return false
}

func excludedName(parts []string, i int) bool {
	part := parts[i]
	return strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") ||
		i < len(parts)-1 && (part == "testdata" || part == "vendor" && i < len(parts)-2)
}

func excludedDir(root, dir string, patterns []string) (bool, error) {
	if ignoredDir(dir, patterns) {
		return true, nil
	}
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir), "go.mod"))
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("%s: inspect module: %w", dir, err)
	}
	return false, nil
}

func excluded(root, name string, patterns []string) (bool, error) {
	parts := strings.Split(name, "/")
	for i := range parts {
		if excludedName(parts, i) {
			return true, nil
		}
		if i == len(parts)-1 {
			break
		}
		skip, err := excludedDir(root, strings.Join(parts[:i+1], "/"), patterns)
		if skip || err != nil {
			return skip, err
		}
	}
	return false, nil
}

func toolchainTags() (map[string]bool, error) {
	out, err := goCommand("tool", "dist", "list")
	if err != nil {
		return nil, err
	}
	tags := make(map[string]bool)
	for _, word := range strings.Fields(fixedTags) {
		tags[word] = true
	}
	for _, pair := range strings.Fields(string(out)) {
		parts := strings.Split(pair, "/")
		for _, part := range parts {
			tags[part] = true
		}
		if len(parts) == 2 {
			tags["arch:"+parts[1]] = true
		}
	}
	return tags, nil
}

func isCustom(tag string, known map[string]bool) bool {
	prefix, _, feature := strings.Cut(tag, ".")
	if known[tag] || releaseTag.MatchString(tag) || strings.HasPrefix(tag, "goexperiment.") {
		return false
	}
	// go help buildconstraint defines feature tags as GOARCH.feature (for example amd64.v2).
	return !feature || !known["arch:"+prefix]
}

func customTags(expr constraint.Expr, known map[string]bool) ([]string, error) {
	pending := []constraint.Expr{expr}
	found := make(map[string]bool)
	for n := 0; len(pending) > 0 && n < maxNodes; n++ {
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch x := node.(type) {
		case *constraint.TagExpr:
			if isCustom(x.Tag, known) {
				found[x.Tag] = true
			}
		case *constraint.NotExpr:
			pending = append(pending, x.X)
		case *constraint.AndExpr:
			pending = append(pending, x.X, x.Y)
		case *constraint.OrExpr:
			pending = append(pending, x.X, x.Y)
		}
	}
	if len(pending) != 0 {
		return nil, errors.New("constraint exceeds node limit")
	}
	tags := make([]string, 0, len(found))
	for tag := range found {
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	return tags, nil
}

func firstPlusBuild(first, text string) string {
	if first == "" && constraint.IsPlusBuild(text) {
		return text
	}
	return first
}

func fileConstraint(path string) (constraint.Expr, string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	var text string
	for _, group := range f.Comments {
		for _, comment := range group.List {
			if comment.Pos() >= f.Package {
				continue
			}
			if constraint.IsGoBuild(comment.Text) {
				expr, err := constraint.Parse(comment.Text)
				return expr, comment.Text, err
			}
			text = firstPlusBuild(text, comment.Text)
		}
	}
	if text != "" {
		return nil, "", fmt.Errorf("// +build constraint %q has no //go:build line; add one (gofmt adds it)", text)
	}
	return nil, "", nil
}

func match(path string, tags []string) (bool, error) {
	ctxt := build.Default
	ctxt.BuildTags = tags
	return ctxt.MatchFile(filepath.Dir(path), filepath.Base(path))
}

func selectedTags(tags []string, mask int) []string {
	selected := make([]string, 0, len(tags))
	for i, tag := range tags {
		if mask&(1<<i) != 0 {
			selected = append(selected, tag)
		}
	}
	return selected
}

func verifyCoverage(path, line string, tags []string) error {
	if len(tags) > maxTags {
		return fmt.Errorf("%s: %s: exceeds %d custom tags", path, line, maxTags)
	}
	none, err := match(path, nil)
	if err != nil {
		return err
	}
	all, err := match(path, tags)
	if err != nil {
		return err
	}
	if none || all {
		return nil
	}
	for mask := 0; mask < 1<<len(tags); mask++ {
		selected := selectedTags(tags, mask)
		ok, err := match(path, selected)
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%s: constraint %s is not covered by either scan", path, line)
		}
	}
	return nil
}

func scanFile(root, name string, known map[string]bool, all map[string]bool) error {
	path := filepath.Join(root, name)
	expr, line, err := fileConstraint(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if expr == nil {
		return nil
	}
	tags, err := customTags(expr, known)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if len(tags) == 0 {
		return nil
	}
	if err := verifyCoverage(path, line, tags); err != nil {
		return err
	}
	for _, tag := range tags {
		all[tag] = true
	}
	return nil
}

func discoveryInputs(root string) ([]string, []string, map[string]bool, error) {
	files, err := gitCommand("-C", root, "ls-files", "-z", "--", "*.go")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: not a readable git work tree: %w", root, err)
	}
	if err := checkInheritedTags(); err != nil {
		return nil, nil, nil, err
	}
	patterns, err := moduleIgnores(root)
	if err != nil {
		return nil, nil, nil, err
	}
	known, err := toolchainTags()
	if err != nil {
		return nil, nil, nil, err
	}
	names := strings.Split(strings.TrimSuffix(string(files), "\x00"), "\x00")
	if len(names) > maxFiles {
		return nil, nil, nil, fmt.Errorf("%s: exceeds file limit", root)
	}
	return names, patterns, known, nil
}

func scanNames(root string, names, patterns []string, known map[string]bool) (map[string]bool, error) {
	all := make(map[string]bool)
	for _, name := range names {
		if name == "" {
			continue
		}
		skip, err := excluded(root, name, patterns)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		if err := scanFile(root, name, known, all); err != nil {
			return nil, err
		}
	}
	return all, nil
}

func discover(root string) ([]string, error) {
	names, patterns, known, err := discoveryInputs(root)
	if err != nil {
		return nil, err
	}
	all, err := scanNames(root, names, patterns, known)
	if err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(all))
	for tag := range all {
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	return tags, nil
}

func main() {
	root := flag.String("root", ".", "module root")
	flag.Parse()
	tags, err := discover(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(strings.Join(tags, ","))
}
