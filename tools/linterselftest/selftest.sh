#!/usr/bin/env bash
# Plants findings in a temporary directory and requires the named scanner to fail on each,
# so the tree scan that follows it in ci.yml is proven to catch them.
# Usage: selftest.sh gosec|semgrep|spectral|trivy|osv-scanner   (run from the repository root; gosec
# runs Makefile's gosec target with .gosec.json and tools/go/go.mod, semgrep reads
# .config/semgrep/hiss.yml, spectral reads .spectral.yaml and needs tools/spectral installed,
# osv-scanner reads osv.dev online with tools/osvscanner/go.mod and osv-scanner.toml)
set -euo pipefail
tool="${1:?usage: selftest.sh gosec|semgrep|spectral|trivy|osv-scanner}"
scratch="$(mktemp -d)"
trap 'rm -r "$scratch"' EXIT
plant="$scratch/plant"
mkdir "$plant"
log="$plant/scan.log"

require_findings() {
  local label="$1" got_rc="$2" scan_log="$3"
  shift 3
  if [[ "$got_rc" -ne 1 ]]; then
    echo "$label: planted findings gave exit $got_rc, want 1" >&2
    cat "$scan_log" >&2
    exit 1
  fi
  local missing=0 id
  for id in "$@"; do
    if ! grep -q -- "$id" "$scan_log"; then
      echo "$label: planted finding $id not reported" >&2
      missing=1
    fi
  done
  if [[ "$missing" -ne 0 ]]; then
    exit 1
  fi
}

run_make_gosec() {
  local out="$1"
  shift
  local make_rc=0
  # make pastes $(GOTOOL) unquoted; pass the modfile path via env so blanks do not split.
  # shellcheck disable=SC2016 # $$ expands in make and the recipe shell, not here.
  LC_ALL=C MAKEFLAGS="" GOSEC_SELFTEST_MODFILE="$modfile" \
    make -s -C "$plant" -f "$repo/Makefile" gosec \
    GOTOOL='go tool -modfile="$$GOSEC_SELFTEST_MODFILE"' "$@" > "$out" 2>&1 || make_rc="$?"
  # GNU make exits 2 whenever a target fails; map recipe exit 1 ("Error 1") back to 1 only when
  # gosec printed Results: (not when go list failed or zero args printed usage), so 1 = findings.
  if [[ "$make_rc" -eq 2 ]] && grep -qE '(\[|: )gosec\] Error 1$' "$out" && grep -q '^Results:' "$out"; then
    return 1
  fi
  return "$make_rc"
}

init_module() {
  if ! go -C "$1" mod init "$2" > "$log" 2>&1; then
    cat "$log" >&2
    exit 1
  fi
}

case "$tool" in
  gosec)
    repo="$PWD"
    moddir="$(mktemp -d "$scratch/go tools.XXXXXX")"
    cp "$repo/tools/go/go.mod" "$repo/tools/go/go.sum" "$moddir/"
    modfile="$moddir/go.mod"
    cp "$repo/.gosec.json" "$plant/.gosec.json"
    mkdir -p \
      "$plant/real" \
      "$plant/xclaude" \
      "$plant/notes.claude" \
      "$plant/dots...mid" \
      "$plant/.claude/worktrees/w" \
      "$plant/.standards/worktrees/gate-1" \
      "$plant/.hidden/probe"
    init_module "$plant" planted
    # A: built package in the root module -> must be reported (G501).
    printf '%s\n' 'package real' '' 'import _ "crypto/md5"' > "$plant/real/real.go"
    # B: folder name without a leading dot is a built package -> must stay scanned (G502).
    printf '%s\n' 'package xclaude' '' 'import _ "crypto/des"' > "$plant/xclaude/b.go"
    # C: dot inside a folder name is a built package -> must stay scanned (G503).
    printf '%s\n' 'package notesclaude' '' 'import _ "crypto/rc4"' > "$plant/notes.claude/c.go"
    # D: nested coding-client worktree with its own module -> must be excluded (G504).
    init_module "$plant/.claude/worktrees/w" nestedclaude
    printf '%s\n' 'package nestedclaude' '' 'import _ "net/http/cgi"' \
      > "$plant/.claude/worktrees/w/nested.go"
    # E: nested Praetor gate worktree with its own module -> must be excluded (G505).
    init_module "$plant/.standards/worktrees/gate-1" nestedstandards
    printf '%s\n' 'package nestedstandards' '' 'import _ "crypto/sha1"' \
      > "$plant/.standards/worktrees/gate-1/nested.go"
    # F: dot state folder with no go.mod holding a stray .go file -> must be excluded (G108).
    printf '%s\n' 'package probe' '' 'import _ "net/http/pprof"' \
      > "$plant/.hidden/probe/probe.go"
    # G: module-root package -> must stay scanned (G404).
    printf '%s\n' 'package planted' '' 'import "math/rand"' '' \
      'func weakRandom() int { return rand.Int() }' > "$plant/g.go"
    # H: three dots inside a folder name -> must stay scanned as itself (G306).
    printf '%s\n' 'package dotsmid' '' 'import "os"' '' \
      'func weakPermissions() error { return os.WriteFile("plant", nil, 0o777) }' \
      > "$plant/dots...mid/h.go"
    expected=(G501 G502 G503 G404 G306)
    rc=0
    run_make_gosec "$log" || rc="$?"
    ;;
  semgrep)
    cat > "$plant/planted.go" <<'GO'
package planted

import (
	"context"
	"net"
	"net/http"
	"os/exec"
)

func planted(ctx context.Context) {
	_ = exec.Command("true").Run()
	_, _ = net.Dial("tcp", "127.0.0.1:1")
	_, _ = http.Get("http://127.0.0.1:1")
	_ = context.TODO()
	_ = exec.CommandContext(ctx, "sh", "-c", "true").Run()
}
GO
    printf '%s\n' '#!/usr/bin/env bash' 'cmd="ls"' "eval \"\$cmd\"" > "$plant/planted.sh"
    expected=(hiss-02-go-exec-without-context hiss-02-go-dial-without-context
      hiss-02-go-http-without-context hiss-02-go-context-todo hiss-08-go-shell-command
      hiss-08-bash-eval)
    set +e
    semgrep scan --config .config/semgrep/hiss.yml --error --metrics=off --disable-version-check \
      --json "$plant" > "$log" 2>&1
    rc=$?
    set -e
    ;;
  spectral)
    # An OpenAPI document without its required info object and with a reference to a schema it
    # does not define.
    cat > "$plant/planted-openapi.yaml" <<'YAML'
openapi: 3.1.0
paths:
  /models:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Missing"
YAML
    expected=(oas3-schema invalid-ref)
    set +e
    tools/spectral/node_modules/.bin/spectral lint -F warn -f json --ruleset .spectral.yaml \
      "$plant/planted-openapi.yaml" > "$log" 2>&1
    rc=$?
    set -e
    ;;
  trivy)
    # A Dockerfile without a USER line (DS-0002, HIGH) and a GitHub token (CRITICAL). The token
    # is assembled at run time so the repository itself holds no secret-shaped string.
    printf '%s\n' 'FROM alpine:3.20' 'RUN true' > "$plant/Dockerfile"
    printf 'token=ghp_%s\n' "$(printf 'a1B2c3D4e5%.0s' 1 2 3 4)" > "$plant/planted.env"
    expected=(DS-0002 github-pat)
    set +e
    trivy fs --scanners misconfig,secret --exit-code 1 --severity HIGH,CRITICAL --quiet \
      --skip-version-check --disable-telemetry --format json "$plant" > "$log" 2>&1
    rc=$?
    set -e
    ;;
  osv-scanner)
    # A: the real lock with fast-uri set back to 3.1.8 (GHSA-w74x-v6f9-j3xg, fixed in 3.1.9)
    # must fail the scan under the repository's osv-scanner.toml. B: the real lock under a copy
    # of osv-scanner.toml whose effectiveUntil lies in the past must report the picomatch advisory
    # again. Exit 1 means advisories found; 0 or 2 and above (scanner error) fails the self-test.
    repo="$PWD"
    modfile="$repo/tools/osvscanner/go.mod"
    old_version="3.1.8"
    old_integrity="sha512-GZMtZUTNRpOVIECoXwLNZS5xUGE+mVNbTB8h/7Rwh2TFWcBQiPzTgyZi05BF9UMZKkLJv8XBRJTlU7zg8+ZfMg=="
    python3 - "$repo/tools/spectral/package-lock.json" "$plant/package-lock.json" \
      "$old_version" "$old_integrity" <<'PY'
import json
import sys

src, dst, version, integrity = sys.argv[1:5]
with open(src, encoding="utf-8") as fh:
    lock = json.load(fh)
entry = lock["packages"]["node_modules/fast-uri"]
entry["version"] = version
entry["resolved"] = "https://registry.npmjs.org/fast-uri/-/fast-uri-" + version + ".tgz"
entry["integrity"] = integrity
with open(dst, "w", encoding="utf-8") as fh:
    json.dump(lock, fh, indent=2)
    fh.write("\n")
PY
    expected=(GHSA-w74x-v6f9-j3xg)
    set +e
    go tool -modfile="$modfile" osv-scanner scan --lockfile "$plant/package-lock.json" \
      --config "$repo/osv-scanner.toml" > "$log" 2>&1
    rc=$?
    set -e
    require_findings "$tool (fast-uri $old_version planted)" "$rc" "$log" "${expected[@]}"
    rm "$plant/package-lock.json"

    expired_log="$plant/expired-scan.log"
    expired_config="$plant/osv-scanner.toml"
    sed 's/^effectiveUntil = .*/effectiveUntil = 2020-01-01/' "$repo/osv-scanner.toml" > "$expired_config"
    if ! grep -q '^effectiveUntil = 2020-01-01$' "$expired_config"; then
      echo "$tool: could not rewrite effectiveUntil in osv-scanner.toml" >&2
      exit 1
    fi
    expected=(GHSA-fc5q-39xc-vjpf)
    set +e
    go tool -modfile="$modfile" osv-scanner scan --lockfile "$repo/tools/spectral/package-lock.json" \
      --config "$expired_config" > "$expired_log" 2>&1
    rc=$?
    set -e
    require_findings "$tool (expired exemption)" "$rc" "$expired_log" "${expected[@]}"
    echo "$tool: planted fast-uri $old_version reported (GHSA-w74x-v6f9-j3xg, exit 1), expired exemption reported (GHSA-fc5q-39xc-vjpf, exit 1)"
    exit 0
    ;;
  *)
    echo "unknown tool: $tool" >&2
    exit 2
    ;;
esac

require_findings "$tool" "$rc" "$log" "${expected[@]}"

if [[ "$tool" == "gosec" ]]; then
  if ! grep -F -- "$plant/g.go:" "$log" | grep -F -- ' - G404 ('; then
    echo "$tool: G (module root) finding G404 not reported in $plant/g.go" >&2
    exit 1
  fi
  if ! grep -F -- "$plant/dots...mid/h.go:" "$log" | grep -F -- ' - G306 ('; then
    echo "$tool: H (mid-name dots) finding G306 not reported in $plant/dots...mid/h.go" >&2
    exit 1
  fi
  unexpected=0
  for forbidden in G504 G505 G108 .claude/worktrees .standards/worktrees .hidden/probe; do
    if grep -qF -- "$forbidden" "$log"; then
      echo "$tool: excluded tree leaked '$forbidden' into scan output" >&2
      unexpected=1
    fi
  done
  if [[ "$unexpected" -ne 0 ]]; then
    exit 1
  fi

  neg_log="$plant/neg-scan.log"
  neg_rc=0
  run_make_gosec "$neg_log" "GOSEC_PACKAGES=echo ./..." || neg_rc="$?"
  require_findings "$tool (GOSEC_PACKAGES='echo ./...')" "$neg_rc" "$neg_log" G504 G505 G108

  # I: folder name ending in a dot -> go list must fail before gosec runs.
  mkdir "$plant/trail..."
  printf '%s\n' 'package trail' > "$plant/trail.../i.go"
  trailing_log="$plant/trailing-scan.log"
  trailing_rc=0
  run_make_gosec "$trailing_log" || trailing_rc="$?"
  if [[ "$trailing_rc" -ne 2 ]] || grep -q '^Results:' "$trailing_log" || \
    ! grep -qF 'malformed import path' "$trailing_log"; then
    echo "$tool: I (trailing-dot folder) gave exit $trailing_rc, want 2, malformed import path, no Results:" >&2
    cat "$trailing_log" >&2
    exit 1
  fi
  rm -r "$plant/trail..."

  rm -r "$plant/real" "$plant/xclaude" "$plant/notes.claude" "$plant/dots...mid"
  rm "$plant/g.go"
  mkdir -p "$plant/clean"
  printf '%s\n' 'package clean' > "$plant/clean/clean.go"
  clean_log="$plant/clean-scan.log"
  clean_rc=0
  run_make_gosec "$clean_log" || clean_rc="$?"
  if [[ "$clean_rc" -ne 0 ]]; then
    echo "$tool: clean tree with excluded folders gave exit $clean_rc, want 0" >&2
    cat "$clean_log" >&2
    exit 1
  fi

  echo "$tool: ${#expected[@]} planted findings reported (exit 1), G module root and H mid-name dots scanned, nested worktrees and dot folders excluded, negative control reported excluded findings (exit 1), I trailing-dot folder rejected before scan (exit 2), clean tree passed (exit 0)"
else
  echo "$tool: ${#expected[@]} planted findings reported, exit 1 as required"
fi
