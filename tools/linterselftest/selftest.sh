#!/usr/bin/env bash
# Plants findings in a temporary directory and requires the named scanner to fail on each,
# so the tree scan that follows it in ci.yml is proven to catch them.
# Usage: selftest.sh gosec|semgrep|spectral|trivy|osv-scanner   (run from the repository root; gosec
# runs Makefile's gosec target with .gosec.json and tools/go/go.mod, semgrep reads
# .config/semgrep/hiss.yml, spectral reads .spectral.yaml and needs tools/spectral installed,
# osv-scanner reads osv.dev online with tools/osvscanner/go.mod and osv-scanner.toml)
set -euo pipefail
# Repository Python launcher (3.10 floor); absolute, because a mode may change directory.
launcher="$PWD/.config/lefthook/python.sh"
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

osv_reported() {
  local scan_log="$1" id="$2" package="$3" version="$4"
  awk -F '|' -v id="$id" -v package="$package" -v version="$version" '
    function trim(s) { sub(/^[[:space:]]+/, "", s); sub(/[[:space:]]+$/, "", s); return s }
    /^\|/ {
      for (i = 2; i < NF; i++) {
        cell = trim($i)
        if (cell == "OSV URL") url_col = i
        if (cell == "PACKAGE") package_col = i
        if (cell == "VERSION") version_col = i
      }
      if (url_col && package_col && version_col &&
          trim($url_col) == "https://osv.dev/" id &&
          trim($package_col) == package && trim($version_col) == version) found = 1
    }
    END { exit !found }
  ' "$scan_log"
}

osv_filtered() {
  local scan_log="$1" id="$2"
  awk -v id="$id" '
    index($0, id " ") == 1 {
      rest = substr($0, length(id) + 2)
      if (rest ~ /^has been filtered out because: / ||
          rest ~ /^and 1 alias have been filtered out because: / ||
          rest ~ /^and [0-9]+ aliases have been filtered out because: /) found = 1
    }
    END { exit !found }
  ' "$scan_log"
}

osv_unused_ignore() {
  local scan_log="$1" id="$2"
  awk -v id="$id" '
    /has unused ignores:$/ { bullets = 1; next }
    bullets && /^ - / { if ($0 == " - " id) found = 1; next }
    { bullets = 0 }
    END { exit !found }
  ' "$scan_log"
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
  # Only map a failed recipe to findings if every failed pass printed its own Results section.
  if [[ "$make_rc" -eq 2 ]] && grep -qE '(\[|: )gosec\] Error 1$' "$out" &&
    awk '/^gosec pass: / { results = 0 } /^Results:/ { results = 1 }
         /^gosec pass failed: / { failed++; if (!results) bad = 1 }
         END { exit !(failed && !bad) }' "$out"; then
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

# Reads the end date of the one osv-scanner.toml exemption. Prints the raw value read on line 1
# ("<unread>" before any read); on a refusal exits 1 and prints the reason on line 2. A file with no
# [[IgnoredVulns]] table (no exemption) prints "absent" and exits 3. Python comes from the repository
# launcher (3.10 floor, so no TOML library): a narrow line reader, not a TOML parser.
toml_exemption_date() {
  sh "$launcher" - "$1" <<'PY'
import datetime
import re
import sys

allowed = ("id", "effectiveUntil", "reason")
tables, ids, untils = [], [], []


def fail(why):
    shown = untils[0] if untils else "<unread>"
    print(f"{shown}\n{why}")
    sys.exit(1)


try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        lines = fh.read().splitlines()
except (OSError, ValueError) as exc:
    fail(f"osv-scanner.toml unreadable ({exc})")
for raw in lines:
    ln = raw.strip()
    if not ln or ln.startswith("#"):
        continue
    if '"""' in ln or "'''" in ln:
        fail("osv-scanner.toml: multi-line strings are not read; keep each value on one line")
    if ln.startswith("["):
        tables.append(re.sub(r"\s+#.*$", "", ln))
        continue
    key, _, value = ln.partition("=")
    key, value = key.strip(), value.strip()
    if not tables:
        fail(f"osv-scanner.toml: key {key} before the [[IgnoredVulns]] table")
    if key not in allowed:
        fail(f"osv-scanner.toml: key {key} not allowed (only effectiveUntil ends the exemption)")
    if key == "id":
        ids.append(value.split("#")[0].strip().strip('"'))
    if key == "effectiveUntil":
        untils.append(value.split("#")[0].strip().strip('"'))
if not tables:
    print("absent")
    sys.exit(3)
if tables != ["[[IgnoredVulns]]"]:
    fail(f"osv-scanner.toml must hold exactly one [[IgnoredVulns]] table and no other; got {tables}")
if len(ids) != 1 or not re.fullmatch(r"GHSA(-[23456789cfghjmpqrvwx]{4}){3}", ids[0]):
    fail(f"osv-scanner.toml must hold exactly one id line in GHSA format; got {ids}")
if len(untils) != 1:
    fail(f"osv-scanner.toml must hold exactly one effectiveUntil line, got {len(untils)}")
if not re.fullmatch(r"\d{4}-\d{2}-\d{2}", untils[0]):
    fail("osv-scanner.toml effectiveUntil is not a YYYY-MM-DD date")
try:
    datetime.date.fromisoformat(untils[0])
except ValueError as exc:
    fail(f"osv-scanner.toml effectiveUntil is not a date ({exc})")
print(untils[0])
PY
}

# Reads the expires date of the .standards.yaml exceptions entry for rule backlog.caps.forge_alerts,
# with the same output contract as toml_exemption_date ("absent", exit 3: no such entry).
yaml_exemption_date() {
  sh "$launcher" - "$1" <<'PY'
import datetime
import re
import sys

rule = "backlog.caps.forge_alerts"
found = []


def fail(why):
    shown = found[0] if found else "<unread>"
    print(f"{shown}\n{why}")
    sys.exit(1)


try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        lines = fh.read().splitlines()
except (OSError, ValueError) as exc:
    fail(f".standards.yaml unreadable ({exc})")
hits = [i for i, ln in enumerate(lines) if rule in ln and not ln.lstrip().startswith("#")]
if not hits:
    print("absent")
    sys.exit(3)
if len(hits) != 1 or lines[hits[0]].rstrip() != f"  - rule: {rule}":
    fail(f"want exactly one '  - rule: {rule}' entry in .standards.yaml, got {len(hits)}")
for ln in lines[hits[0] + 1:]:
    if re.match(r"  - |\S", ln):
        break
    match = re.fullmatch(r'    expires: "?(\d{4}-\d{2}-\d{2})"?\s*(#.*)?', ln)
    if match:
        found.append(match.group(1))
if len(found) != 1:
    fail(f"want exactly one expires line in the {rule} entry, got {len(found)}")
try:
    datetime.date.fromisoformat(found[0])
except ValueError as exc:
    fail(f".standards.yaml expires is not a date ({exc})")
print(found[0])
PY
}

# Decides the production exemption state offline. Both files hold the entry: prints the shared date,
# exit 0, or exits 1 naming both values when the dates differ. Neither holds one: prints nothing,
# returns 3. Exactly one holds one, or a reader refuses: exits 1 naming the file.
shared_exemption_date() {
  local toml_out yaml_out toml_rc=0 yaml_rc=0 why="" gone=""
  toml_out="$(toml_exemption_date "$1")" || toml_rc="$?"
  yaml_out="$(yaml_exemption_date "$2")" || yaml_rc="$?"
  local toml_val="${toml_out%%$'\n'*}" yaml_val="${yaml_out%%$'\n'*}"
  if [[ "$toml_rc" -eq 3 && "$toml_out" == absent ]]; then toml_val="<absent>"; gone+="t"; toml_rc=0; fi
  if [[ "$yaml_rc" -eq 3 && "$yaml_out" == absent ]]; then yaml_val="<absent>"; gone+="y"; yaml_rc=0; fi
  if [[ "$toml_rc" -ne 0 ]]; then
    why="${toml_out#*$'\n'}"
    [[ "$toml_out" == *$'\n'* ]] || { toml_val="<unread>"; why="osv-scanner.toml reader exit $toml_rc"; }
  elif [[ "$yaml_rc" -ne 0 ]]; then
    why="${yaml_out#*$'\n'}"
    [[ "$yaml_out" == *$'\n'* ]] || { yaml_val="<unread>"; why=".standards.yaml reader exit $yaml_rc"; }
  elif [[ "$gone" == ty || "$gone" == yt ]]; then
    return 3
  elif [[ "$gone" == t ]]; then
    why="osv-scanner.toml has no exemption entry but .standards.yaml has one"
  elif [[ "$gone" == y ]]; then
    why=".standards.yaml has no exceptions entry but osv-scanner.toml has an exemption"
  elif [[ "$toml_val" != "$yaml_val" ]]; then
    why="dates differ"
  fi
  if [[ -n "$why" ]]; then
    echo "osv-scanner: exemption dates: $why: osv-scanner.toml effectiveUntil=${toml_val:-<unread>}" \
      ".standards.yaml expires=${yaml_val:-<unread>}" >&2
    exit 1
  fi
  echo "$yaml_val"
}

# Writes an osv-scanner config exempting an advisory until the given date.
# Args: file, date (YYYY-MM-DD), advisory id.
osv_write_config() {
  printf '%s\n' '[[IgnoredVulns]]' "id = \"$3\"" "effectiveUntil = $2" \
    'reason = "self-test plant"' > "$1"
}

# Copies a lockfile with one package forced to a version. Args: source lock, destination lock,
# package name, version, integrity. Adds the entry when the source lock has none.
osv_plant_lock() {
  sh "$launcher" - "$@" <<'PY'
import json
import sys

src, dst, name, version, integrity = sys.argv[1:6]
with open(src, encoding="utf-8") as fh:
    lock = json.load(fh)
entry = lock["packages"].setdefault("node_modules/" + name, {})
entry["version"] = version
entry["resolved"] = "https://registry.npmjs.org/" + name + "/-/" + name + "-" + version + ".tgz"
entry["integrity"] = integrity
with open(dst, "w", encoding="utf-8") as fh:
    json.dump(lock, fh, indent=2)
    fh.write("\n")
PY
}

# Runs osv-scanner on a lockfile under a config and returns its exit code. Args: lock, config, log.
osv_scan() {
  local scan_rc=0
  go tool -modfile="$modfile" osv-scanner scan --lockfile "$1" --config "$2" > "$3" 2>&1 || scan_rc="$?"
  return "$scan_rc"
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
    # J-N: distinguish test files, each tag pass, fully tagged packages, and malformed nosec.
    mkdir -p "$plant/only" "$plant/negative"
    printf '%s\n' 'package real' 'import _ "crypto/md5"' > "$plant/real/j_test.go"
    printf '%s\n' '//go:build planttag' '' 'package real' 'import _ "crypto/des"' > "$plant/real/k.go"
    printf '%s\n' '//go:build !planttag' '' 'package real' 'import _ "crypto/rc4"' > "$plant/real/l.go"
    printf '%s\n' '//go:build plantonly' '' 'package only' 'import _ "crypto/md5"' > "$plant/only/m.go"
    printf '%s\n' 'package negative' 'import _ "crypto/md5" // #nosec G501' > "$plant/negative/n1.go"
    printf '%s\n' 'package negative' 'import _ "crypto/des" // #nosec -- missing rule' > "$plant/negative/n2.go"
    # Strip inherited Git variables and disable global and system config for the fixture.
    fixture_git() {
      python3 - "$plant" "$@" <<'PYGIT'
import os
import subprocess
import sys

GIT_TIMEOUT_SECONDS = 30
MAX_ENV_VARS = 65536
if len(os.environ) > MAX_ENV_VARS:
    raise RuntimeError("fixture environment exceeds variable limit")
env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
env["GIT_CONFIG_GLOBAL"] = os.devnull
env["GIT_CONFIG_NOSYSTEM"] = "1"
subprocess.run(["git", "-C", sys.argv[1], *sys.argv[2:]], env=env, check=True, timeout=GIT_TIMEOUT_SECONDS)
PYGIT
    }
    fixture_git init -q
    fixture_git add -- .
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
    # C (offline, first): the production exemption state. Both osv-scanner.toml and .standards.yaml
    # hold an entry with matching dates and no other advisory is exempt. Neither holds one: skipped.
    # Exactly one: fail. A: the real lock with fast-uri set back to 3.1.8 (GHSA-w74x-v6f9-j3xg,
    # fixed in 3.1.9) must fail the scan under the repository's osv-scanner.toml. B2 and B scan the
    # real lock with braces 3.0.2 planted (GHSA-grv7-fg5c-xmjg), without the production config.
    # B2: a config that ended 2020-01-01 must report the advisory and must not filter it. B: a
    # config valid until 2099-01-01 must exit 0 without it and must filter it; any other advisory
    # fails this case. B2 runs first: when it does not report the advisory, osv.dev withdrew it or
    # changed its range, and the self-test asks for a new plant instead of reading B's unused
    # ignore as a report. Exit 1 means advisories found in A and B2; B needs exit 0; any other
    # exit (scanner error) fails the self-test. The checks read three line shapes of osv-scanner
    # v2.7.1: the report table row (OSV URL, package, version), the "filtered out" line and the
    # bullets under "has unused ignores:".
    repo="$PWD"
    modfile="$repo/tools/osvscanner/go.mod"
    shared_rc=0
    shared_date="$(shared_exemption_date "$repo/osv-scanner.toml" "$repo/.standards.yaml")" || shared_rc="$?"
    case "$shared_rc" in
      0) date_note="exemption dates agree on $shared_date" ;;
      3) date_note="no production exemption; date check skipped" ;;
      *) exit 1 ;;
    esac
    echo "$tool: $date_note"
    lock="$repo/tools/spectral/package-lock.json"
    mkdir "$plant/a" "$plant/b"
    old_version="3.1.8"
    old_integrity="sha512-GZMtZUTNRpOVIECoXwLNZS5xUGE+mVNbTB8h/7Rwh2TFWcBQiPzTgyZi05BF9UMZKkLJv8XBRJTlU7zg8+ZfMg=="
    # braces 3.0.2 (GHSA-grv7-fg5c-xmjg, fixed in 3.0.3): integrity of that npm tarball, fixed here
    # so the plant still works after the real lock moves to another braces version.
    braces_version="3.0.2"
    braces_integrity="sha512-b8um+L1RzM3WDSzvhm6gIz1yfTbBt6YTlcEKAvsmqCZZFw46z626lVj9j1yEPW33H5H+lBQpZMP1k8l+78Ha0A=="
    braces_advisory="GHSA-grv7-fg5c-xmjg"
    osv_plant_lock "$lock" "$plant/a/package-lock.json" fast-uri "$old_version" "$old_integrity" || {
      echo "$tool: could not plant fast-uri $old_version" >&2
      exit 1
    }
    osv_plant_lock "$lock" "$plant/b/package-lock.json" braces "$braces_version" "$braces_integrity" || {
      echo "$tool: could not plant braces $braces_version" >&2
      exit 1
    }
    expected=(GHSA-w74x-v6f9-j3xg)
    rc=0
    osv_scan "$plant/a/package-lock.json" "$repo/osv-scanner.toml" "$log" || rc="$?"
    require_findings "$tool (fast-uri $old_version planted)" "$rc" "$log" "${expected[@]}"
    if ! osv_reported "$log" GHSA-w74x-v6f9-j3xg fast-uri "$old_version"; then
      echo "$tool (fast-uri $old_version planted): advisory GHSA-w74x-v6f9-j3xg not reported for fast-uri $old_version" >&2
      cat "$log" >&2
      exit 1
    fi

    expired_log="$plant/expired-scan.log"
    osv_write_config "$plant/expired.toml" 2020-01-01 "$braces_advisory"
    rc=0
    osv_scan "$plant/b/package-lock.json" "$plant/expired.toml" "$expired_log" || rc="$?"
    if osv_filtered "$expired_log" "$braces_advisory"; then
      echo "$tool (expired exemption): expired exemption honoured: osv-scanner filtered $braces_advisory under a config that ended 2020-01-01" >&2
      cat "$expired_log" >&2
      exit 1
    fi
    if [[ "$rc" -le 1 ]] && ! osv_reported "$expired_log" "$braces_advisory" braces "$braces_version"; then
      echo "$tool (expired exemption): planted advisory $braces_advisory is no longer reported by osv.dev (withdrawn or its range changed); a new plant is needed" >&2
      cat "$expired_log" >&2
      exit 1
    fi
    require_findings "$tool (expired exemption)" "$rc" "$expired_log" "$braces_advisory"

    valid_log="$plant/valid-scan.log"
    osv_write_config "$plant/valid.toml" 2099-01-01 "$braces_advisory"
    rc=0
    osv_scan "$plant/b/package-lock.json" "$plant/valid.toml" "$valid_log" || rc="$?"
    if osv_unused_ignore "$valid_log" "$braces_advisory"; then
      echo "$tool (valid exemption): unused ignore $braces_advisory; the plant no longer matches the advisory" >&2
      cat "$valid_log" >&2
      exit 1
    fi
    if [[ "$rc" -ne 0 ]] || osv_reported "$valid_log" "$braces_advisory" braces "$braces_version"; then
      echo "$tool (valid exemption): exit $rc; the planted braces $braces_version lock under a valid exemption must exit 0 without $braces_advisory" >&2
      cat "$valid_log" >&2
      exit 1
    fi
    if ! osv_filtered "$valid_log" "$braces_advisory"; then
      echo "$tool (valid exemption): valid exemption not applied: no filtered-out line for $braces_advisory" >&2
      cat "$valid_log" >&2
      exit 1
    fi

    echo "$tool: planted fast-uri $old_version reported (GHSA-w74x-v6f9-j3xg, exit 1), expired exemption reported on planted braces $braces_version ($braces_advisory, exit 1), valid exemption honoured ($braces_advisory absent, exit 0), $date_note"
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

  for file in real/j_test.go real/k.go real/l.go only/m.go negative/n1.go negative/n2.go; do
    if ! grep -qF -- "$plant/$file:" "$log"; then
      echo "$tool: missing planted finding $file" >&2
      exit 1
    fi
  done
  for control in tests tags nosec; do
    case "$control" in
      tests) override='GOSEC_TESTS='; missed=(real/j_test.go) ;;
      tags) override='GOSEC_TAGS=printf ""'; missed=(real/k.go only/m.go) ;;
      nosec) override='GOSEC_NOSEC='; missed=(negative/n1.go negative/n2.go) ;;
    esac
    control_rc=0
    run_make_gosec "$plant/control-$control.log" "$override" || control_rc="$?"
    if [[ "$control_rc" -ne 1 ]]; then
      echo "$tool: $control control exited $control_rc, want findings" >&2
      exit 1
    fi
    for file in "${missed[@]}"; do
      if grep -qF -- "$plant/$file:" "$plant/control-$control.log"; then
        echo "$tool: $control control unexpectedly found $file" >&2
        exit 1
      fi
    done
  done

  # O: a file reachable only under an intermediate assignment must be refused.
  printf '%s\n' '//go:build planttag && !plantother' '' 'package real' > "$plant/real/o.go"
  fixture_git add -- real/o.go
  gap_rc=0
  run_make_gosec "$plant/gap.log" || gap_rc="$?"
  if [[ "$gap_rc" -ne 2 ]] || ! grep -qF 'real/o.go' "$plant/gap.log" || grep -q '^Results:' "$plant/gap.log"; then
    echo "$tool: O gap refusal failed (exit $gap_rc)" >&2
    cat "$plant/gap.log" >&2
    exit 1
  fi
  rm "$plant/real/o.go"
  fixture_git add -A

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

  rm -r "$plant/real" "$plant/xclaude" "$plant/notes.claude" "$plant/dots...mid" "$plant/only" "$plant/negative"
  rm "$plant/g.go"
  mkdir -p "$plant/clean"
  printf '%s\n' 'package clean' > "$plant/clean/clean.go"
  fixture_git add -A
  clean_log="$plant/clean-scan.log"
  clean_rc=0
  run_make_gosec "$clean_log" || clean_rc="$?"
  if [[ "$clean_rc" -ne 0 ]]; then
    echo "$tool: clean tree with excluded folders gave exit $clean_rc, want 0" >&2
    cat "$clean_log" >&2
    exit 1
  fi

  # P and Q: isolate each pass so losing either pass's exit status is observable.
  for case_id in P Q; do
    if [[ "$case_id" == P ]]; then
      printf '%s\n' '//go:build !planttag' '' 'package clean' 'import _ "crypto/md5"' > "$plant/clean/negative.go"
      printf '%s\n' '//go:build planttag' '' 'package clean' > "$plant/clean/positive.go"
    else
      printf '%s\n' '//go:build !planttag' '' 'package clean' > "$plant/clean/negative.go"
      printf '%s\n' '//go:build planttag' '' 'package clean' 'import _ "crypto/md5"' > "$plant/clean/positive.go"
    fi
    fixture_git add -- clean/negative.go clean/positive.go
    pass_rc=0
    run_make_gosec "$plant/pass-$case_id.log" || pass_rc="$?"
    if [[ "$case_id" == P ]]; then
      wanted='gosec pass failed: tests'
      forbidden='^gosec pass failed: tests tags='
    else
      wanted='gosec pass failed: tests tags=planttag'
      forbidden='^gosec pass failed: tests$'
    fi
    if [[ "$pass_rc" -ne 1 ]] || ! grep -qxF "$wanted" "$plant/pass-$case_id.log" ||
      grep -q "$forbidden" "$plant/pass-$case_id.log"; then
      echo "$tool: $case_id isolated pass failed (exit $pass_rc)" >&2
      cat "$plant/pass-$case_id.log" >&2
      exit 1
    fi
  done

  echo "$tool: ${#expected[@]} planted findings reported (exit 1), G module root and H mid-name dots scanned, nested worktrees and dot folders excluded, negative control reported excluded findings (exit 1), I trailing-dot folder rejected, J-N found, controls miss their plants, O refused, P and Q isolated passes detected, clean tree passed (exit 0)"
else
  echo "$tool: ${#expected[@]} planted findings reported, exit 1 as required"
fi
