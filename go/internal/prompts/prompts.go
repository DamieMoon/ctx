// Package prompts is the identity register of the prompt bodies this tree
// sends to a model: ID -> {Version, Owner}, and nothing else. It holds no
// prompt TEXT (E04-5 option A, design/04 §8): every body stays a constant in
// the package whose parser reads its answer, because the binding between a
// prompt and the code that decodes its output is a compile-time one
// (events/distill_extract.go's DisallowUnknownFields is the sharpest example —
// a body an operator could edit at runtime would leave that parser silently
// empty). What was missing was not a home for the text, it was an ANSWER to
// "which prompt stand produced this context_llm_log row" — thirty days of rows
// and no way to say which of them came from which prompt generation.
//
// Each body declares itself next to its own text:
//
//	var promptClassify = prompts.Register(
//		"llm.classifySystemPrompt", "2026-06-13", "github.com/GottZ/ctx/internal/llm")
//
// and the pipeline that sends it stamps the returned Identity into
// llmlog.Entry.Metadata (prompt_id / prompt_version, jsonb — no migration).
// The ID is <package>.<identifier>, so a row names a place in the tree rather
// than a label someone has to look up; Owner is the full import path, which
// the registry test verifies against the package the Register call lives in.
//
// Registration happens during package initialisation and a duplicate ID
// PANICS: two bodies under one id would make every row carrying it
// unattributable, and a process that cannot say what it sent should not start.
//
// Stdlib-only by construction (the leaf doctrine of design/01; the depguard
// rule that enforces it lands in T06-7): a package that every prompt-carrying
// package imports must not be able to import any of them back.
package prompts

import (
	"fmt"
	"sort"
	"sync"
)

// Identity is what a row can say about the prompt that produced it: which
// body (ID), which generation of it (Version), and which package owns the
// text (Owner, the full import path).
//
// Version is the body's own vocabulary where the code already has one — the
// synthesis pair carries v5.2/v6, the two values config key
// query.prompt_version accepts — and the ISO date of the commit that last
// changed the body text everywhere else. Those dates are AUTHOR dates
// (git log --format=%as), because that is when the wording was written; a
// cherry-pick moves the committer date and would silently renumber a body
// nobody touched. It is a LABEL, not a checksum: the body-hash pin in this
// package's tests is what keeps an edited body from keeping its old version.
type Identity struct {
	ID      string
	Version string
	Owner   string
}

var (
	mu       sync.Mutex
	registry = map[string]Identity{}
)

// Register records one prompt body and returns its Identity, so the call site
// can keep the value in a package-level var and hand it to the pipeline that
// sends the body.
//
// It panics on an empty field and on a duplicate ID. Both are init-time
// programmer errors — the alternative, a zero Identity travelling into an
// llmlog row, is a row that claims a provenance it does not have.
func Register(id, version, owner string) Identity {
	if id == "" || version == "" || owner == "" {
		panic(fmt.Sprintf("prompts: incomplete registration id=%q version=%q owner=%q", id, version, owner))
	}
	mu.Lock()
	defer mu.Unlock()
	if prev, ok := registry[id]; ok {
		panic(fmt.Sprintf("prompts: duplicate id %q (already registered as version %q by %s)", id, prev.Version, prev.Owner))
	}
	ident := Identity{ID: id, Version: version, Owner: owner}
	registry[id] = ident
	return ident
}

// All returns every registered identity, sorted by ID. The order is fixed so
// a gate can compare two runs, and the slice is a copy: the register is
// written once, during init, and read for the rest of the process.
func All() []Identity {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Identity, 0, len(registry))
	for _, ident := range registry {
		out = append(out, ident)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Lookup returns the identity registered under id.
func Lookup(id string) (Identity, bool) {
	mu.Lock()
	defer mu.Unlock()
	ident, ok := registry[id]
	return ident, ok
}
