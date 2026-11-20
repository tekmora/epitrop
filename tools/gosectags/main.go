// Command gosectags discovers build tags and verifies two-pass scan coverage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
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

func command(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).Output() // #nosec G204 -- callers select git or go with fixed arguments; root is a git -C argument
	if err != nil {
		return nil, fmt.Errorf("%s: %w", args[0], err)
	}
	return out, nil
}

func toolchainTags() (map[string]bool, error) {
	out, err := command("go", "tool", "dist", "list")
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

func fileConstraint(path string) (constraint.Expr, string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		return nil, "", err
	}
	for _, group := range f.Comments {
		for _, comment := range group.List {
			if comment.Pos() >= f.Package {
				continue
			}
			if constraint.IsGoBuild(comment.Text) {
				expr, err := constraint.Parse(comment.Text)
				return expr, comment.Text, err
			}
		}
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

func discover(root string) ([]string, error) {
	files, err := command("git", "-C", root, "ls-files", "-z", "--", "*.go")
	if err != nil {
		return nil, fmt.Errorf("%s: not a readable git work tree: %w", root, err)
	}
	known, err := toolchainTags()
	if err != nil {
		return nil, err
	}
	names := strings.Split(strings.TrimSuffix(string(files), "\x00"), "\x00")
	if len(names) > maxFiles {
		return nil, fmt.Errorf("%s: exceeds file limit", root)
	}
	all := make(map[string]bool)
	for _, name := range names {
		if name == "" {
			continue
		}
		if err := scanFile(root, name, known, all); err != nil {
			return nil, err
		}
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
