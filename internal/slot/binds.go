package slot

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/tekmora/epitrop/internal/config"
)

// Bind modes: the workspace is bound for writing, inputs and a home's bind files for
// reading, and a home's copy files are copied in before the start.
const (
	BindWrite = "rw"
	BindRead  = "ro"
	BindCopy  = "copy"
)

// MaxBranchChanges bounds the changes the branch step reads from a run's commits.
const MaxBranchChanges = 1 << 16

// Bind is one path the job's sandbox makes visible to the job: Source on the host at Target
// in the sandbox. SHA256 is the digest a copied file had to have.
type Bind struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256,omitempty"`
}

// Binder is what a job that knows its sandbox's binds implements.
type Binder interface {
	Binds() []Bind
}

// jobBinds returns the binds of a job that knows them.
func jobBinds(job Job) []Bind {
	binder, ok := job.(Binder)
	if !ok {
		return nil
	}
	return binder.Binds()
}

// readOnlyFiles are the sources of the read-only binds that are regular files: the files a
// run can read and must not hand back in its branch. A bound directory is not looked into.
func readOnlyFiles(binds []Bind) []string {
	files := make([]string, 0, len(binds))
	for i := 0; i < len(binds); i++ {
		if binds[i].Mode != BindRead {
			continue
		}
		info, err := os.Lstat(binds[i].Source)
		// An empty file gives nothing away, and its content is any empty file's.
		if err == nil && info.Mode().IsRegular() && info.Size() > 0 && !slices.Contains(files, binds[i].Source) {
			files = append(files, binds[i].Source)
		}
	}
	return files
}

// checkBranchContent refuses a branch whose commits, any of them, touch the home directory
// or add a file whose content equals a read-only bound file. The check reads every commit
// between base and head, merges against each parent, so a file added and removed again is
// found as well as one that stays.
//
// It finds a bound file copied as it is. A run that changes the bytes before it commits them
// is not found here; that is why a home binds only what the client needs.
func checkBranchContent(ctx context.Context, repo string, base string, head string, bound []string) error {
	blobs := make(map[string]string, len(bound))
	for i := 0; i < len(bound); i++ {
		id, err := git(ctx, repo, "hash-object", "--no-filters", "--", bound[i])
		if err != nil {
			return fmt.Errorf("hash the bound file %s: %w", bound[i], err)
		}
		blobs[id] = bound[i]
	}
	out, err := git(ctx, repo, "log", "--format=", "--raw", "--no-abbrev", "-z", "--no-renames", "--diff-merges=separate", base+".."+head)
	if err != nil {
		return fmt.Errorf("list the changes of %s..%s: %w", base, head, err)
	}
	return checkRawChanges(strings.Split(out, "\x00"), blobs)
}

// checkRawChanges reads git's raw diff records, each ":<modes> <old> <new> <status>" and the
// path, and refuses a path in the home directory and a new blob of a bound file.
func checkRawChanges(fields []string, blobs map[string]string) error {
	if len(fields) > 2*MaxBranchChanges {
		return fmt.Errorf("the branch holds more than %d changes", MaxBranchChanges)
	}
	for i := 0; i < len(fields); i++ {
		meta := strings.TrimSpace(fields[i])
		if meta == "" {
			continue
		}
		parts := strings.Fields(meta)
		if !strings.HasPrefix(meta, ":") || len(parts) != 5 || i+1 >= len(fields) {
			return fmt.Errorf("unexpected change record %q", meta)
		}
		i++
		path := fields[i] // #nosec G602 -- the preceding i+1 length check bounds the incremented index
		if path == config.SandboxHomeTop || strings.HasPrefix(path, config.SandboxHomeTop+"/") {
			return fmt.Errorf("a commit touches %s, the job's home; nothing there is the run's work", path)
		}
		if source, ok := blobs[parts[3]]; ok {
			return fmt.Errorf("a commit adds %s with the content of the bound file %s", path, source)
		}
	}
	return nil
}
