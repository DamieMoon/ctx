package config

import (
	"sort"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"
)

// retireV3MigrationFile is the delete migration of the THIRD retirement
// vintage (Tree-Shaking-Nachzug wave NZ-5). Named as a constant for the reason
// its two siblings are: three tests read it, and a renamed file must fail
// loudly here rather than silently stop being checked.
const retireV3MigrationFile = "153_retire_v3_settings.sql"

// retireV3RequestID is the audit marker the migration stamps onto every unset
// it causes (SET LOCAL ctx.request_id). It is quoted verbatim in the runbook's
// recovery query (docs/operations.md, "Migration 153") and in the migration's
// own third notice, so it is pinned rather than left to drift.
const retireV3RequestID = "migration-153-retire-v3-settings"

// arrayLiteral and quotedString are shared with retiredmigration_test.go —
// same package, same file shape, and a third copy of those two regexes would
// be exactly the kind of duplicate transcript this file exists to prevent.

// TestRetireV3MigrationDeletesExactlyTheRetiredKeys pins the second transcript
// of the third vintage's key names against the first, the way its two siblings
// do it for the backend tuple and for the keys without a successor.
// config.retiredKeysWithoutSuccessorV3 is the one Go-side source, but a SQL
// migration cannot call a Go function — the list has to exist a second time
// inside the .sql file, and the two drift in both directions with different
// damage:
//
//   - a key MISSING from the array leaves its rows behind on every foreign
//     installation. They are invisible after the registry cut (HandleList is
//     registry-driven, referencedBy filters on config.Keys(), a tenant-scoped
//     orphan does not even reach the global reload's WARN) and become live
//     configuration again the day someone re-registers the name.
//   - a key TOO MANY in the array deletes rows of a LIVING key on every
//     installation that runs the upgrade — silent config loss, no error. The
//     living neighbours of this vintage are one character apart from two of
//     its members (distill.ctx_source_label, distill.ctx_quiet_for), which is
//     what makes the over-listing direction more than theoretical here.
//
// Set equality catches both. Unit test on purpose: it reads the embedded FS,
// needs no database, and therefore runs in the `-short` loop that guards every
// commit.
func TestRetireV3MigrationDeletesExactlyTheRetiredKeys(t *testing.T) {
	sql := readRetireV3Migration(t)

	match := arrayLiteral.FindStringSubmatch(sql)
	if match == nil {
		t.Fatalf("%s: no ARRAY[ … ] literal found — the delete migration must carry the key list as an array literal, or this pin stops pinning anything", retireV3MigrationFile)
	}

	var fromSQL []string
	for _, q := range quotedString.FindAllStringSubmatch(match[1], -1) {
		fromSQL = append(fromSQL, q[1])
	}
	sort.Strings(fromSQL)

	want := RetiredV3KeyNames() // sorted by contract
	if len(fromSQL) != len(want) {
		t.Fatalf("%s: array lists %d key(s), config.RetiredV3KeyNames() has %d\n array: %v\n   map: %v",
			retireV3MigrationFile, len(fromSQL), len(want), fromSQL, want)
	}
	for i := range want {
		if fromSQL[i] != want[i] {
			t.Errorf("%s: array[%d] = %q, config.RetiredV3KeyNames()[%d] = %q — the two transcripts of this vintage have drifted apart",
				retireV3MigrationFile, i, fromSQL[i], i, want[i])
		}
	}

	// Duplicate guard: a doubled entry would keep the counts matching against
	// a shorter map only by accident, and it would make the migration's own
	// intent unreadable.
	seen := make(map[string]bool, len(fromSQL))
	for _, k := range fromSQL {
		if seen[k] {
			t.Errorf("%s: key %q listed more than once in the array", retireV3MigrationFile, k)
		}
		seen[k] = true
	}
}

// TestRetireV3MigrationCarriesTheAuditMarker pins the one handle the migration
// leaves on the values it deletes. Like the vintage before it there is no
// upgrade-hop hint to pin — these keys have no successor and no hop; the
// marker is the whole message. The runbook quotes it verbatim, so a drifted
// marker turns the documented recovery query into one that returns nothing.
func TestRetireV3MigrationCarriesTheAuditMarker(t *testing.T) {
	sql := readRetireV3Migration(t)

	if !strings.Contains(sql, "SET LOCAL ctx.request_id = '"+retireV3RequestID+"'") {
		t.Errorf("%s: audit marker %q not set — without it the deleted values are unattributable and the runbook's recovery query returns nothing",
			retireV3MigrationFile, retireV3RequestID)
	}

	// The marker has to reach the operator too: it is quoted inside the
	// migration's own notice, which is the only place a running upgrade names
	// it. A marker set but never raised leaves the operator with a value he
	// cannot find.
	if !raisesNoticeContaining(sql, retireV3RequestID) {
		t.Errorf("%s: %q is set but never raised — an operator reading the boot log has no way to reach the deleted values",
			retireV3MigrationFile, retireV3RequestID)
	}
}

// TestRetireV3MigrationNoticeStaysInItsOwnVintage keeps the copy-paste vintage
// confusion out of the RAISED text. This file was written from
// 152_retire_v2_settings.sql, which in turn was written from 133 — so two
// foreign vintages can bleed through, and every statement of both is FALSE
// here: these keys disappear in v5.17.0, not in v5.16.0 and not in the v5
// major; no pool owns their values; the "migrate to last 4.x" hop belongs to a
// migration these keys never met.
//
// The vintage-own claim of THIS migration is the second one: the arm's second
// source keeps two keys whose names are one segment apart from two of the
// deleted ones (distill.ctx_source_label, distill.ctx_quiet_for), and an
// operator who reads them as renames would copy a dead label into a live
// watermark series. The notice therefore has to say the opposite out loud, and
// this test pins that it does — a notice that only lists what went is exactly
// the notice that invites the mistake.
//
// Only RAISE lines are inspected: the comment head deliberately cites 152 as
// its build pattern, which is provenance for a reader of the file and reaches
// no operator.
func TestRetireV3MigrationNoticeStaysInItsOwnVintage(t *testing.T) {
	sql := readRetireV3Migration(t)

	if !raisesNoticeContaining(sql, "v5.17.0") {
		t.Errorf("%s: no raised notice names the release the keys disappear in (v5.17.0) — the operator cannot tell which upgrade removed his row", retireV3MigrationFile)
	}

	if !raisesNoticeContaining(sql, "distill.ctx_source_label") || !raisesNoticeContaining(sql, "distill.ctx_quiet_for") {
		t.Errorf("%s: no raised notice names the two living neighbours — without them the operator's most likely repair is to copy a deleted value into a live watermark series", retireV3MigrationFile)
	}

	for _, wrong := range []string{"migrate to last 4.x", "v5.0.0", "v5.16.0", "context_backends", "ctx backends", "Migration 133", "Migration 152"} {
		if raisesNoticeContaining(sql, wrong) {
			t.Errorf("%s: a raised notice carries %q — that belongs to an earlier vintage, and these keys have neither its successor nor its hop",
				retireV3MigrationFile, wrong)
		}
	}
}

func readRetireV3Migration(t *testing.T) string {
	t.Helper()
	raw, err := migrations.FS.ReadFile(retireV3MigrationFile)
	if err != nil {
		t.Fatalf("read %s from the embedded migration FS: %v", retireV3MigrationFile, err)
	}
	return string(raw)
}
