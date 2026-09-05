#!/usr/bin/env bash
#
# ctx — deadcode allowlist gate
# Usage: ./deadcode.sh
#
# Compares the reachability analysis of golang.org/x/tools/cmd/deadcode against
# go/deadcode-allow.txt. Two failure directions, both blocking:
#
#   * a symbol that is unreachable but NOT in the allowlist  -> cut it, or write
#     a line with a reason (the reason column is mandatory, so "keeping it" is a
#     visible decision in the diff rather than a matter of discipline);
#   * an allowlist line whose symbol became reachable again  -> delete the line.
#     Without this direction an allowlist only ever grows and turns into a lid.
#
# deadcode is structurally blind to constants, vars, types and struct fields;
# that class is covered by the `unused` linter in .golangci.yml, whose escape
# hatch is `//nolint:unused // <reason>` — same doctrine, other tool.
#
# ctx — Your AI's save game. By GottZ (github.com/GottZ/ctx/graphs/contributors)

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/go"

# The package set is hard-wired, never taken from the caller: deadcode reports
# per-package reachability, so `deadcode ./internal/armsweep/` alone flags nine
# symbols whose callers live in cmd/ — a fehlalarm generator. Modes are a closed
# list for the same reason; T02-13 adds `without-tests` here (a second flag set
# against a second allowlist), it does not open the script up to arguments.
MODE="${1:-with-tests}"
case "$MODE" in
    with-tests)
        DEADCODE_FLAGS=(-test -tags=integration)
        ALLOW_SRC="deadcode-allow.txt"
        ;;
    *)
        echo "deadcode-gate: unknown mode '$MODE' (known: with-tests)" >&2
        echo "  the package set is not configurable — see the comment above." >&2
        exit 2
        ;;
esac

PKGS=(./cmd/... ./internal/... ./migrations/...)

if ! command -v deadcode >/dev/null 2>&1; then
    echo "deadcode-gate: deadcode not found in PATH."
    echo "  install: go install golang.org/x/tools/cmd/deadcode@v0.49.0"
    if [[ -n "${CI:-}" ]]; then
        # In CI a missing tool must not pass as a green gate — CI is the
        # authority (the local hook is only the early warning).
        echo "deadcode-gate: CI is set — refusing to report ok without running." >&2
        exit 1
    fi
    exit 0
fi

RAW="$(mktemp)"; IST="$(mktemp)"; ALLOW="$(mktemp)"
trap 'rm -f "$RAW" "$IST" "$ALLOW"' EXIT

# deadcode runs on its OWN line with a redirection, never inside a pipe: under
# `set -o pipefail` a clean tree makes the downstream grep exit 1 and the gate
# goes red although everything is fine; without pipefail a deadcode type error
# (rc != 0 on stderr) gets masked and the gate goes green on a broken run. The
# redirection lets `set -e` see the real exit code either way.
deadcode "${DEADCODE_FLAGS[@]}" "${PKGS[@]}" > "$RAW"

# Both `grep -v` filters carry `|| true` because an EMPTY result is the success
# case here (nothing unreachable / an allowlist made of comments only), and grep
# signals "no lines" with rc 1 — which would kill the script under `set -e`
# exactly when it should report ok. This is not the masking described above:
# the deadcode run itself is already checked, on its own line.
# The node_modules filter is a second line of defence only: the explicit package
# set above cannot reach go/web/** in the first place.
{ grep -v 'web/node_modules/' "$RAW" || true; } \
    | sed 's/^\([^:]*\):[0-9]*:[0-9]*: unreachable func: /\1\t/' \
    | LC_ALL=C sort -u > "$IST"

# Comparison key is field 1 + field 2 (path + symbol); the reason column is not
# part of it, and no line number is either — otherwise every shift inside a file
# would redden the gate.
{ grep -v '^[[:space:]]*\(#\|$\)' "$ALLOW_SRC" || true; } \
    | cut -f1,2 | LC_ALL=C sort -u > "$ALLOW"

NEW="$(comm -23 "$IST" "$ALLOW")"
GONE="$(comm -13 "$IST" "$ALLOW")"

RC=0

if [[ -n "$NEW" ]]; then
    echo "deadcode-gate: NEW unreachable symbol(s) not in the allowlist:"
    printf '%s\n' "$NEW" | sed 's/^/  /'
    echo ""
    echo "  Either cut the symbol, or add a line to go/$ALLOW_SRC:"
    echo "    <path-relative-to-go><TAB><symbol><TAB># <reason>"
    RC=1
fi

if [[ -n "$GONE" ]]; then
    echo "deadcode-gate: allowlist entr(ies) no longer unreachable — delete the line:"
    printf '%s\n' "$GONE" | sed 's/^/  /'
    RC=1
fi

if [[ $RC -eq 0 ]]; then
    echo "deadcode-gate: ok ($(wc -l < "$ALLOW" | tr -d ' ') entries, all allowlisted)"
fi

exit $RC
