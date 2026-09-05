package config

import (
	"sort"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"
)

// retireV2MigrationFile is the delete migration of the SECOND retirement
// vintage (Tree-Shaking-Run wave T05-10). Named as a constant for the reason
// its sibling in retiredmigration_test.go is: three tests read it, and a
// renamed file must fail loudly here rather than silently stop being checked.
const retireV2MigrationFile = "152_retire_v2_settings.sql"

// retireV2RequestID is the audit marker the migration stamps onto every unset
// it causes (SET LOCAL ctx.request_id). It is quoted verbatim in the runbook's
// recovery query (docs/operations.md, "Migration 152") and in the migration's
// own second notice, so it is pinned rather than left to drift.
const retireV2RequestID = "migration-152-retire-v2-settings"

// arrayLiteral and quotedString are shared with retiredmigration_test.go —
// same package, same file shape, and a second copy of those two regexes would
// be exactly the kind of duplicate transcript this file exists to prevent.

// TestRetireV2MigrationDeletesExactlyTheRetiredKeys pins the second
// transcript of the second vintage's key names against the first, the way
// TestRetireMigrationDeletesExactlyTheRetiredKeys does it for the backend
// tuple. config.retiredKeysWithoutSuccessor is the one Go-side source, but a
// SQL migration cannot call a Go function — the list has to exist a second
// time inside the .sql file, and the two drift in both directions with
// different damage:
//
//   - a key MISSING from the array leaves its rows behind on every foreign
//     installation. They are invisible after the registry cut (HandleList is
//     registry-driven, referencedBy filters on config.Keys(), a tenant-scoped
//     orphan does not even reach the global reload's WARN) and become live
//     configuration again the day someone re-registers the name.
//   - a key TOO MANY in the array deletes rows of a LIVING key on every
//     installation that runs the upgrade — silent config loss, no error.
//
// Set equality catches both. Unit test on purpose: it reads the embedded FS,
// needs no database, and therefore runs in the `-short` loop that guards
// every commit.
func TestRetireV2MigrationDeletesExactlyTheRetiredKeys(t *testing.T) {
	sql := readRetireV2Migration(t)

	match := arrayLiteral.FindStringSubmatch(sql)
	if match == nil {
		t.Fatalf("%s: no ARRAY[ … ] literal found — the delete migration must carry the key list as an array literal, or this pin stops pinning anything", retireV2MigrationFile)
	}

	var fromSQL []string
	for _, q := range quotedString.FindAllStringSubmatch(match[1], -1) {
		fromSQL = append(fromSQL, q[1])
	}
	sort.Strings(fromSQL)

	want := RetiredV2KeyNames() // sorted by contract
	if len(fromSQL) != len(want) {
		t.Fatalf("%s: array lists %d key(s), config.RetiredV2KeyNames() has %d\n array: %v\n   map: %v",
			retireV2MigrationFile, len(fromSQL), len(want), fromSQL, want)
	}
	for i := range want {
		if fromSQL[i] != want[i] {
			t.Errorf("%s: array[%d] = %q, config.RetiredV2KeyNames()[%d] = %q — the two transcripts of this vintage have drifted apart",
				retireV2MigrationFile, i, fromSQL[i], i, want[i])
		}
	}

	// Duplicate guard: a doubled entry would keep the counts matching against
	// a shorter map only by accident, and it would make the migration's own
	// intent unreadable.
	seen := make(map[string]bool, len(fromSQL))
	for _, k := range fromSQL {
		if seen[k] {
			t.Errorf("%s: key %q listed more than once in the array", retireV2MigrationFile, k)
		}
		seen[k] = true
	}
}

// TestRetireV2MigrationCarriesTheAuditMarker pins the one handle the migration
// leaves on the values it deletes. Unlike the first vintage there is no
// upgrade-hop hint to pin here — these keys have no successor and no hop; the
// marker is the whole message. The runbook quotes it verbatim, so a drifted
// marker turns the documented recovery query into one that returns nothing.
func TestRetireV2MigrationCarriesTheAuditMarker(t *testing.T) {
	sql := readRetireV2Migration(t)

	if !strings.Contains(sql, "SET LOCAL ctx.request_id = '"+retireV2RequestID+"'") {
		t.Errorf("%s: audit marker %q not set — without it the deleted values are unattributable and the runbook's recovery query returns nothing",
			retireV2MigrationFile, retireV2RequestID)
	}

	// The marker has to reach the operator too: it is quoted inside the
	// migration's own notice, which is the only place a running upgrade names
	// it. A marker set but never raised leaves the operator with a value he
	// cannot find.
	if !raisesNoticeContaining(sql, retireV2RequestID) {
		t.Errorf("%s: %q is set but never raised — an operator reading the boot log has no way to reach the deleted values",
			retireV2MigrationFile, retireV2RequestID)
	}
}

// TestRetireV2MigrationNoticeStaysInItsOwnVintage keeps the copy-paste
// vintage confusion out of the RAISED text. This file was written from
// 133_retire_backend_tuple_rows.sql, and three of that file's statements are
// FALSE here: the keys of this vintage disappear in v5.16.0 and not in the v5
// major, no pool owns their values, and the "migrate to last 4.x" hop belongs
// to a migration this key never met. The boot sweep of the same vintage holds
// exactly these three negatives (cmd/ctxd/retiredsources_integration_test.go,
// TestWarnRetiredV2SettingRowsBoot) — the migration is the second surface
// speaking to the same operator, and it must not send him to a runbook
// section about a backend tuple.
//
// Only RAISE lines are inspected: the comment head deliberately cites 133 as
// its build pattern, which is provenance for a reader of the file and reaches
// no operator.
func TestRetireV2MigrationNoticeStaysInItsOwnVintage(t *testing.T) {
	sql := readRetireV2Migration(t)

	if !raisesNoticeContaining(sql, "v5.16.0") {
		t.Errorf("%s: no raised notice names the release the keys disappear in (v5.16.0) — the operator cannot tell which upgrade removed his row", retireV2MigrationFile)
	}

	for _, wrong := range []string{"migrate to last 4.x", "v5.0.0", "context_backends", "ctx backends", "Migration 133"} {
		if raisesNoticeContaining(sql, wrong) {
			t.Errorf("%s: a raised notice carries %q — that belongs to the backend-tuple vintage (Migration 133), and these keys have no successor to point at",
				retireV2MigrationFile, wrong)
		}
	}
}

// raisesNoticeContaining reports whether the migration RAISEs a message
// carrying needle. A SQL comment does not count: a comment reaches nobody at
// migration time.
func raisesNoticeContaining(sql, needle string) bool {
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.Contains(trimmed, "RAISE NOTICE") && strings.Contains(trimmed, needle) {
			return true
		}
	}
	return false
}

func readRetireV2Migration(t *testing.T) string {
	t.Helper()
	raw, err := migrations.FS.ReadFile(retireV2MigrationFile)
	if err != nil {
		t.Fatalf("read %s from the embedded migration FS: %v", retireV2MigrationFile, err)
	}
	return string(raw)
}
