#!/usr/bin/env bash
# Plants findings in a temporary directory and requires the named scanner to fail on each,
# so the tree scan that follows it in ci.yml is proven to catch them.
# Usage: selftest.sh gosec|semgrep|spectral|trivy   (run from the repository root; gosec runs
# Makefile's gosec target with .gosec.json and tools/go/go.mod, semgrep reads
# .config/semgrep/hiss.yml, spectral reads .spectral.yaml and needs tools/spectral installed)
set -euo pipefail
tool="${1:?usage: selftest.sh gosec|semgrep|spectral|trivy}"
plant="$(mktemp -d)"
trap 'rm -r "$plant"' EXIT
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
  LC_ALL=C MAKEFLAGS="" make -s -C "$plant" -f "$repo/Makefile" gosec \
    GOTOOL="go tool -modfile=$repo/tools/go/go.mod" "$@" > "$out" 2>&1 || make_rc="$?"
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
    cp "$repo/.gosec.json" "$plant/.gosec.json"
    mkdir -p \
      "$plant/real" \
      "$plant/xclaude" \
      "$plant/notes.claude" \
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
    expected=(G501 G502 G503)
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
  *)
    echo "unknown tool: $tool" >&2
    exit 2
    ;;
esac

require_findings "$tool" "$rc" "$log" "${expected[@]}"

if [[ "$tool" == "gosec" ]]; then
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

  rm -r "$plant/real" "$plant/xclaude" "$plant/notes.claude"
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

  echo "$tool: ${#expected[@]} planted findings reported (exit 1), nested worktrees and dot folders excluded, negative control reported excluded findings (exit 1), clean tree passed (exit 0)"
else
  echo "$tool: ${#expected[@]} planted findings reported, exit 1 as required"
fi
