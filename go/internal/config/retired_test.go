package config

import (
	"sort"
	"strings"
	"testing"
)

// retiredKeysGolden is the static 29-name expectation of the retirement — the
// half of the α15 pin that has to outlive its own subject. Until this wave the
// completeness check compared retiredSettingKeys against the live superseded
// registry set; that expectation evaporates tuple by tuple as the cut waves
// land and says nothing at all once the last one is gone. A hand-written golden
// is the only expectation that still holds after the cut: an edit to the map —
// a lost key would silently shrink the env sweep, the boot row sweep and the
// delete migration's second list, a stray one would sweep a living key — fails
// here without borrowing its truth from the thing being removed.
//
// Grouped by the wave that removes the tuple from the registry, in cut order.
var retiredKeysGolden = []string{
	// β3 — rerank
	"rerank.api_key", "rerank.host", "rerank.model",
	// β4 — chat_fallback
	"chat_fallback.api_key", "chat_fallback.host", "chat_fallback.protocol", "chat_fallback.timeout",
	// β5 — dream_embed
	"dream_embed.api_key", "dream_embed.host", "dream_embed.model", "dream_embed.num_ctx", "dream_embed.protocol",
	// β6 — dream
	"dream.api_key", "dream.host", "dream.model", "dream.num_ctx", "dream.protocol", "dream.think",
	// β7 — embed
	"embed.api_key", "embed.host", "embed.model", "embed.num_ctx", "embed.protocol",
	// β8 — chat
	"chat.api_key", "chat.host", "chat.model", "chat.num_ctx", "chat.protocol", "chat.think",
}

// retiredKeysAlreadyCut carries the retired keys whose registry entry is GONE.
// It is the ratchet of the cut train: every key wave moves its tuple here in
// the same commit that deletes the struct fields, and the pin below refuses
// both halves of a mismatch — a key cut without its line (the registry lookup
// misses where the pin expects a hit) and a line without the cut (the lookup
// hits where the pin expects a miss). The expectation can neither be loosened
// ahead of the removal nor be left stale behind one, which is what turns a
// per-wave count into an invariant.
//
// Empty at β2 by construction: the preparation wave writes the end form, the
// cut waves fill the list (29 → 26 → 22 → 17 → 11 → 6 → 0 registered). β8
// landed the chat tuple, so the ratchet is FULL and this file now asserts
// exactly `Registry ∩ retiredSettingKeys = ∅` plus the full EnvVars()
// inversion — including the chat.host positive probe that no wave before the
// cut could run, since the registry is reflection-built and cannot be faked in
// a test (registry.go).
// Lexically sorted, not in cut order (the pin below requires it) — the wave
// comments name the commit each block arrived with.
var retiredKeysAlreadyCut = []string{
	// β8 — chat
	"chat.api_key", "chat.host", "chat.model", "chat.num_ctx", "chat.protocol", "chat.think",
	// β4 — chat_fallback
	"chat_fallback.api_key", "chat_fallback.host", "chat_fallback.protocol", "chat_fallback.timeout",
	// β6 — dream
	"dream.api_key", "dream.host", "dream.model", "dream.num_ctx", "dream.protocol", "dream.think",
	// β5 — dream_embed
	"dream_embed.api_key", "dream_embed.host", "dream_embed.model", "dream_embed.num_ctx", "dream_embed.protocol",
	// β7 — embed
	"embed.api_key", "embed.host", "embed.model", "embed.num_ctx", "embed.protocol",
	// β3 — rerank
	"rerank.api_key", "rerank.host", "rerank.model",
}

// TestRetiredKeysMatchGoldenList pins the map contents against the static
// 29-name list: the retirement covers exactly the six backend role tuples, no
// more, no less. Together with TestRetiredKeysLeaveTheRegistryWithTheirWave it
// replaces the α15 set-equality against the superseded registry set — same two
// statements (the list is complete, no name on it is a live config key), but
// only the second one still consults a registry that is about to lose these
// keys.
func TestRetiredKeysMatchGoldenList(t *testing.T) {
	golden := map[string]bool{}
	for _, key := range retiredKeysGolden {
		if golden[key] {
			t.Errorf("retiredKeysGolden lists %s twice", key)
		}
		golden[key] = true
	}
	if len(golden) != 29 {
		t.Fatalf("retiredKeysGolden carries %d distinct keys, want 29 (the six backend role tuples)", len(golden))
	}
	if len(retiredSettingKeys) != len(golden) {
		t.Errorf("retiredSettingKeys has %d entries, golden list has %d", len(retiredSettingKeys), len(golden))
	}
	for key := range retiredSettingKeys {
		if !golden[key] {
			t.Errorf("retiredSettingKeys carries %s, which is not in the golden list", key)
		}
	}
	for key := range golden {
		if _, ok := retiredSettingKeys[key]; !ok {
			t.Errorf("golden key %s is missing from retiredSettingKeys", key)
		}
	}
}

// TestRetiredKeysLeaveTheRegistryWithTheirWave is the collision pin in its end
// form, ratcheted through the cut train. The invariant it protects is permanent
// and outlives the cut: no name in retiredSettingKeys is a live config key.
//
// Until β9 the pre-cut half of that sentence read "…or it carries the
// superseded marker for its remaining lifetime". The marker was removed with
// the whole mechanic (E11), and with the ratchet full there is no pre-cut half
// left to legitimise: every retired name must simply be absent from the
// registry. The third arm below therefore no longer inspects a marker — it
// states outright that a registered retired name is a bug.
//
// That is what stops a future release from re-registering one of these names:
// a re-registered name would silently make every stale row on it effective
// configuration again (build.go admits any registered key) and would shadow the
// retirement wherever it is documented.
//
// E13 (404, not 410) is why this file is the whole contract on the config side:
// the retired keys answer through the ordinary unknownKey path, so "retired"
// means precisely "not in the registry" — there is no tombstone response that
// could carry the statement instead.
func TestRetiredKeysLeaveTheRegistryWithTheirWave(t *testing.T) {
	cut := map[string]bool{}
	for _, key := range retiredKeysAlreadyCut {
		if _, ok := retiredSettingKeys[key]; !ok {
			t.Errorf("retiredKeysAlreadyCut lists %s, which is not a retired key at all", key)
		}
		if cut[key] {
			t.Errorf("retiredKeysAlreadyCut lists %s twice", key)
		}
		cut[key] = true
	}
	if !sort.StringsAreSorted(retiredKeysAlreadyCut) {
		t.Errorf("retiredKeysAlreadyCut is not sorted: %v", retiredKeysAlreadyCut)
	}

	for key := range retiredSettingKeys {
		_, registered := KeyByName(key)
		switch {
		case cut[key] && registered:
			t.Errorf("%s is listed as cut but the registry still carries it — "+
				"a retired name back in the registry revives every stale row on it", key)
		case !cut[key] && !registered:
			t.Errorf("%s is gone from the registry but missing from retiredKeysAlreadyCut — "+
				"the cut wave moves its tuple into the ratchet in the same commit", key)
		case !cut[key] && registered:
			t.Errorf("%s is a retired key that is still registered — since β9 removed the "+
				"superseded marker there is no legitimate pre-cut state left; a retired name is gone", key)
		}
	}
}

// TestRetiredEnvNamesFollowTheCut pins the mechanical key → env derivation and
// inverts the α15 EnvVars() assertion wave by wave. Before its cut a retired
// key's derived name must BE the env var the registry reads for it and must be
// part of EnvVars() — that is what lets the boot WARN and the β13 tripwire
// consume a derived list instead of a third transcript of 29 strings. After its
// cut the same name must be ABSENT from EnvVars(): the cut wave is only done
// when the env surface is gone with the key, not just the struct field. With
// the ratchet empty the second half is dormant; with it full this test is the
// per-tuple env absence gate of every key wave (design/01 W3: "EnvVars()
// enthält keine CTX_CHAT_FALLBACK_*").
func TestRetiredEnvNamesFollowTheCut(t *testing.T) {
	live := map[string]bool{}
	for _, name := range EnvVars() {
		live[name] = true
	}
	cut := map[string]bool{}
	for _, key := range retiredKeysAlreadyCut {
		cut[key] = true
	}

	for key := range retiredSettingKeys {
		derived := retiredEnvName(key)
		if info, registered := KeyByName(key); registered && derived != info.EnvVar {
			t.Errorf("%s: derived env name %q, registry tag %q", key, derived, info.EnvVar)
		}
		if cut[key] {
			if live[derived] {
				t.Errorf("%s is cut but %q is still in EnvVars() — the env surface must go with the key", key, derived)
			}
			continue
		}
		if !live[derived] {
			t.Errorf("%s: derived env name %q is not in EnvVars()", key, derived)
		}
	}

	names := RetiredEnvNames()
	if len(names) != len(retiredSettingKeys) {
		t.Errorf("RetiredEnvNames() returned %d names, map has %d keys", len(names), len(retiredSettingKeys))
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("RetiredEnvNames() is not sorted: %v", names)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			t.Errorf("RetiredEnvNames() lists %s twice", name)
		}
		seen[name] = true
	}
}

// TestRetiredTargetsNamePoolLocation pins the map VALUES as usable data: the
// docs, the runbook and the delete migration's notice quote them as the place
// the value moved to, so an empty or unspecific target would ship an operator
// message that answers nothing. The values never reach the wire (E13: the keys
// answer a plain 404), which is exactly why no test elsewhere would catch rot
// in them.
func TestRetiredTargetsNamePoolLocation(t *testing.T) {
	for key, target := range retiredSettingKeys {
		if target == "" {
			t.Errorf("retired key %s has no move target", key)
			continue
		}
		if !strings.Contains(target, "context_backends") {
			t.Errorf("retired key %s: target %q does not name a context_backends location", key, target)
		}
	}
}

// TestGamingKeysStayOutOfTheRetiredList separates the two retirement vintages.
// gaming.active / gaming.disabled_backends were retired in U01-W5: unregistered
// since, never env-sourced, no pool destination — they answer 404 through the
// unknownKey path today and need neither an env sweep nor a move target. The
// backend tuple keys are the newer vintage and reach the same 404 only after
// the registry cut (E13). Putting the gaming keys into retiredSettingKeys would
// invent env vars that never existed (CTX_GAMING_ACTIVE) and feed the tripwire
// a name it can never see set.
func TestGamingKeysStayOutOfTheRetiredList(t *testing.T) {
	for _, key := range []string{"gaming.active", "gaming.disabled_backends"} {
		if _, ok := retiredSettingKeys[key]; ok {
			t.Errorf("%s must not be in retiredSettingKeys (different retirement vintage)", key)
		}
		if _, ok := KeyByName(key); ok {
			t.Errorf("%s is registered again — the U01-W5 retirement expects an unknown key (404)", key)
		}
	}
	for _, name := range RetiredEnvNames() {
		if name == "CTX_GAMING_ACTIVE" || name == "CTX_GAMING_DISABLED_BACKENDS" {
			t.Errorf("RetiredEnvNames() lists %s — the gaming keys never had env vars", name)
		}
	}
}

// TestRetirementVintagesStaySeparate is the same statement one vintage later,
// and it is the pin the second list was given its own file section for.
//
// The two maps make two different claims. retiredSettingKeys says "the value
// lives HERE now" and its 29 are wired into a suffix-filtered env sweep, a
// row sweep that names v5.0.0 and Migration 133, and the 29er pattern of
// retireddocs_test.go. retiredKeysWithoutSuccessor says "the value stopped
// existing, and here is why nothing changes" and is wired into its own env
// sweep without a suffix filter, its own row sweep and its own delete
// migration. A name in both lists would be swept twice with two different
// releases in the text and deleted by two migrations — the operator would read
// two unrelated problems about one row.
//
// Three assertions, three ways the separation can rot:
//
//  1. No name in two maps, and no derived env name in two lists — checked
//     over every PAIR of vintages, not only against the first one. With three
//     lists the interesting collision is the one between the two successorless
//     vintages, which a first-vintage-only check would walk straight past.
//  2. RetiredEnvNames() stays at 29. It is the first vintage's pin, quoted
//     verbatim by retireddocs_test.go, and the only reason a second list
//     exists instead of a longer first one.
//  3. Every V2 and V3 value is a non-empty Ist statement. The map's VALUE is
//     the whole difference to the first vintage, it reaches no wire (E13: the
//     key answers a plain 404), and nothing else would catch rot in it.
func TestRetirementVintagesStaySeparate(t *testing.T) {
	// Every pair, both directions covered by the index pairing. Written over a
	// slice rather than as hand-written pairs so a fourth vintage joins the
	// gate by being added to the list, instead of by somebody remembering to
	// write three more comparisons.
	vintages := []struct {
		name string
		keys map[string]string
		env  []string
	}{
		{"V1", retiredSettingKeys, RetiredEnvNames()},
		{"V2", retiredKeysWithoutSuccessor, RetiredV2EnvNames()},
		{"V3", retiredKeysWithoutSuccessorV3, RetiredV3EnvNames()},
	}
	for i, a := range vintages {
		for _, b := range vintages[i+1:] {
			for key := range b.keys {
				if _, both := a.keys[key]; both {
					t.Errorf("%s is in BOTH %s and %s — one key, two releases in the boot text and two delete migrations",
						key, a.name, b.name)
				}
			}
			inA := map[string]bool{}
			for _, name := range a.env {
				inA[name] = true
			}
			for _, name := range b.env {
				if inA[name] {
					t.Errorf("%s is swept by %s and %s — the env tripwire would log it twice, with two different releases",
						name, a.name, b.name)
				}
			}
		}
	}

	if got := len(RetiredEnvNames()); got != 29 {
		t.Errorf("RetiredEnvNames() returned %d names, want 29 — the second vintage must not grow the first "+
			"(retireddocs_test.go pins the same number, and a key added to the wrong map moves it)", got)
	}

	if got, want := len(RetiredV2KeyNames()), len(retiredKeysWithoutSuccessor); got != want {
		t.Errorf("RetiredV2KeyNames() returned %d keys, map has %d", got, want)
	}
	if got, want := len(RetiredV2EnvNames()), len(retiredKeysWithoutSuccessor); got != want {
		t.Errorf("RetiredV2EnvNames() returned %d names, map has %d", got, want)
	}
	if !sort.StringsAreSorted(RetiredV2KeyNames()) || !sort.StringsAreSorted(RetiredV2EnvNames()) {
		t.Errorf("the V2 lists are not sorted — same diffability contract as the V1 pair")
	}
	v2Env := map[string]bool{}
	for _, name := range RetiredV2EnvNames() {
		v2Env[name] = true
	}
	for _, key := range RetiredV2KeyNames() {
		if derived := retiredEnvName(key); !v2Env[derived] {
			t.Errorf("%s: derived env name %q is not in RetiredV2EnvNames() (%v) — both lists must come from the same derivation",
				key, derived, RetiredV2EnvNames())
		}
	}

	if got, want := len(RetiredV3KeyNames()), len(retiredKeysWithoutSuccessorV3); got != want {
		t.Errorf("RetiredV3KeyNames() returned %d keys, map has %d", got, want)
	}
	if got, want := len(RetiredV3EnvNames()), len(retiredKeysWithoutSuccessorV3); got != want {
		t.Errorf("RetiredV3EnvNames() returned %d names, map has %d", got, want)
	}
	if !sort.StringsAreSorted(RetiredV3KeyNames()) || !sort.StringsAreSorted(RetiredV3EnvNames()) {
		t.Errorf("the V3 lists are not sorted — same diffability contract as the V1 pair")
	}
	v3Env := map[string]bool{}
	for _, name := range RetiredV3EnvNames() {
		v3Env[name] = true
	}
	for _, key := range RetiredV3KeyNames() {
		if derived := retiredEnvName(key); !v3Env[derived] {
			t.Errorf("%s: derived env name %q is not in RetiredV3EnvNames() (%v) — both lists must come from the same derivation",
				key, derived, RetiredV3EnvNames())
		}
	}

	for _, v := range []struct {
		name string
		keys map[string]string
	}{
		{"V2", retiredKeysWithoutSuccessor},
		{"V3", retiredKeysWithoutSuccessorV3},
	} {
		for key, ist := range v.keys {
			if strings.TrimSpace(ist) == "" {
				t.Errorf("%s retired key %s carries no Ist statement — the VALUE is what makes this a separate vintage", v.name, key)
			}
		}
	}
}

// TestRetiredV3KeysLeftTheRegistry is the third vintage's cut gate, and the
// twin of TestRetiredV2KeysLeftTheRegistry: the registry half of "GET
// /api/settings does not serve it any more". The list (handler.HandleList) is
// built from config.Keys(), which walks registry(), so an unregistered key
// cannot appear in the response, in the CLI's settings list or in the web UI.
// A name back in the registry would revive every stale row on it as effective
// configuration.
//
// It also pins the env surface and the description table: the key's variable
// must be gone from EnvVars(), or the cut removed the struct field and left a
// reader behind, and a description without a registered key is a dangling half
// of the same cut.
func TestRetiredV3KeysLeftTheRegistry(t *testing.T) {
	for _, key := range RetiredV3KeyNames() {
		if info, registered := KeyByName(key); registered {
			t.Errorf("%s is registered again (env %q) — a retired name back in the registry is served by "+
				"GET /api/settings and revives every stale row on it", key, info.EnvVar)
		}
	}
	live := map[string]bool{}
	for _, name := range EnvVars() {
		live[name] = true
	}
	for _, name := range RetiredV3EnvNames() {
		if live[name] {
			t.Errorf("%s is still in EnvVars() — the env surface must go with the key", name)
		}
	}
	for _, key := range RetiredV3KeyNames() {
		if _, described := keyDescriptions[key]; described {
			t.Errorf("%s still carries a registry description — a description without a registered key is a dangling half of the cut", key)
		}
	}
}

// TestRetiredV2KeysLeftTheRegistry is the second vintage's cut gate, and the
// registry half of "GET /api/settings does not serve it any more": the list
// (handler.HandleList) is built from config.Keys(), which walks registry(), so
// an unregistered key cannot appear in the response, in the CLI's settings
// list or in the web UI. A name back in the registry would revive every stale
// row on it as effective configuration — the same failure mode the first
// vintage's precondition test names.
//
// It also pins the env surface: the key's variable must be gone from
// EnvVars(), or the cut removed the struct field and left a reader behind.
func TestRetiredV2KeysLeftTheRegistry(t *testing.T) {
	for _, key := range RetiredV2KeyNames() {
		if info, registered := KeyByName(key); registered {
			t.Errorf("%s is registered again (env %q) — a retired name back in the registry is served by "+
				"GET /api/settings and revives every stale row on it", key, info.EnvVar)
		}
	}
	live := map[string]bool{}
	for _, name := range EnvVars() {
		live[name] = true
	}
	for _, name := range RetiredV2EnvNames() {
		if live[name] {
			t.Errorf("%s is still in EnvVars() — the env surface must go with the key", name)
		}
	}
	for _, key := range RetiredV2KeyNames() {
		if _, described := keyDescriptions[key]; described {
			t.Errorf("%s still carries a registry description — a description without a registered key is a dangling half of the cut", key)
		}
	}
}
