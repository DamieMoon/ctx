package handler

import (
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
)

// Welle T02-11 (design/02 §8 E02-4): the container-free half of the
// parent.mode=required gate, at the one function every write surface consults.
//
// WHY A UNIT PROBE NEXT TO mcp_store_gatechain_integration_test.go (subtest i):
// that file proves the gate REACHES the three store arms — it can only do so
// with a database, because the registry snapshot the arms read comes from a
// table. This file proves the VERDICT: which class, which prose, and that
// optional/none/absent stay admissible. The two together are the wave's claim;
// neither alone is.
//
// The fixture carries all three modes, because a gate that refused every type
// would satisfy a probe on `required` alone.
func TestClaimReject_ParentRequired(t *testing.T) {
	// gcParentSet mirrors the shipped shape: a required type (the class
	// `comment` belongs to), an optional one, a none one, plus the default
	// knowledge type so the set is a plausible registry rather than a stub.
	set, err := blocktype.NewSet([]blocktype.Policy{
		{
			Name: "knowledge", Scope: "_global", Builtin: true, IsDefault: true,
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalFullPass},
			Guard: blocktype.GuardPolicy{
				Check: true, Candidate: true,
				Mode: blocktype.GuardModeArchive, Candidates: blocktype.GuardCandidatesAll,
			},
			Dream:  blocktype.DreamPolicy{Linkable: true},
			Parent: blocktype.ParentPolicy{Mode: blocktype.ParentModeNone},
		},
		{
			Name: "parent-required", Scope: "_global", Builtin: true,
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalFullPass},
			Guard: blocktype.GuardPolicy{
				Check: false, Candidate: false,
				Mode: blocktype.GuardModeArchive, Candidates: blocktype.GuardCandidatesAll,
			},
			Parent: blocktype.ParentPolicy{Mode: blocktype.ParentModeRequired, Relationship: "comment-of"},
		},
		{
			Name: "parent-optional", Scope: "_global", Builtin: true,
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalFullPass},
			Guard: blocktype.GuardPolicy{
				Check: false, Candidate: false,
				Mode: blocktype.GuardModeArchive, Candidates: blocktype.GuardCandidatesAll,
			},
			Parent: blocktype.ParentPolicy{Mode: blocktype.ParentModeOptional},
		},
		{
			Name: "parent-none", Scope: "_global", Builtin: true,
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalFullPass},
			Guard: blocktype.GuardPolicy{
				Check: false, Candidate: false,
				Mode: blocktype.GuardModeArchive, Candidates: blocktype.GuardCandidatesAll,
			},
			Parent: blocktype.ParentPolicy{Mode: blocktype.ParentModeNone},
		},
	})
	if err != nil {
		t.Fatalf("fixture set: %v", err)
	}

	// Premise, asserted rather than assumed: without it the refusal below could
	// fire for any other reason and prove nothing about parent.mode.
	if got := set.ParentMode("parent-required"); got != blocktype.ParentModeRequired {
		t.Fatalf("fixture ParentMode(parent-required) = %q, want %q — premise of this file",
			got, blocktype.ParentModeRequired)
	}

	const wantMsg = `type: "parent-required" requires a parent (parent.mode=required) — ` +
		`this write surface carries no parent, so a block of that type is created ` +
		`through its own domain path, not claimed here`

	t.Run("required_type_refused_422", func(t *testing.T) {
		rej := claimReject(set, "test", "parent-required", nil)
		if rej == nil {
			t.Fatal("admissible — a client may claim a required-parent type on a surface with no parent; " +
				"this is E02-4 open (policy without mechanism)")
		}
		if rej.Status != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want %d", rej.Status, http.StatusUnprocessableEntity)
		}
		if rej.Code != "parent_required" {
			t.Errorf("code = %q, want parent_required — a separate code from unknown_type "+
				"(fix the name) and reserved_type (drop the claim): the remedy here is the "+
				"type's own domain path", rej.Code)
		}
		if rej.Msg != wantMsg {
			t.Errorf("prose = %q, want %q", rej.Msg, wantMsg)
		}
	})

	t.Run("optional_and_none_and_absent_stay_admissible", func(t *testing.T) {
		for _, name := range []string{"parent-optional", "parent-none", "knowledge", ""} {
			if rej := claimReject(set, "test", name, nil); rej != nil {
				t.Errorf("type %q refused %s/%q — only parent.mode=required is gated",
					name, rej.Code, rej.Msg)
			}
		}
	})

	t.Run("order_the_type_gates_run_first", func(t *testing.T) {
		// An unknown name must still answer unknown_type, not parent_required:
		// ParentMode falls back to "none" for names it does not know, so a gate
		// placed AHEAD of the membership check would have answered "admissible"
		// for a typo and let it through to the next gate's verdict.
		rej := claimReject(set, "test", "no-such-type", nil)
		if rej == nil || rej.Code != "unknown_type" {
			t.Fatalf("unknown name answered %v, want unknown_type — the membership check "+
				"stays in front of the parent gate", rej)
		}
		// A nil registry stays fail-closed at the membership check; the parent
		// gate never sees an unvalidated name.
		rej = claimReject(nil, "test", "parent-required", nil)
		if rej == nil || rej.Code != "unknown_type" {
			t.Fatalf("nil registry answered %v, want unknown_type (fail-closed)", rej)
		}
	})
}
