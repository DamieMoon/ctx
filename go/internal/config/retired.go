package config

import (
	"sort"
	"strings"
)

// retiredSettingKeys maps every settings key whose living home moved to the F3
// backend pool onto the pool location that owns the value now. It is the ONE
// list of this retirement: the boot WARN over still-set env vars, the tombstone
// tripwire and the docs/runbook sections take their names from here instead of
// each carrying its own transcript of 29 strings (K4 — one tripwire, three
// ingredients).
//
// The map is DATA, never an API surface. Per decision E13 the retired keys
// answer 404 through the ordinary unknownKey path once the registry cut lands —
// no tombstone status, no hint field, nothing on the wire that separates them
// from a typo. The move targets live here so the places that DO carry the
// message (BREAKING tag annotation, runbook, README upgrade-hop paragraph, the
// delete migration's notice) can name one destination, spelled once.
//
// gaming.active / gaming.disabled_backends are deliberately NOT in here. They
// are the older retirement vintage (U01-W5, see the closing comment of Config's
// pool group): never env-sourced, no pool destination, unregistered since their
// cutover — they already answer 404 today and contribute no env name to sweep.
// Every vintage has its own behaviour; retired_test.go pins the separation of
// all of them.
var retiredSettingKeys = map[string]string{
	"chat.host":     "context_backends.base_url (chat-role row)",
	"chat.api_key":  "context_backends.api_key_ref (F2 secret, chat-role row)",
	"chat.protocol": "context_backends.protocol (chat-role row)",
	"chat.model":    "context_backends.model_map (chat role)",
	"chat.num_ctx":  "context_backends.num_ctx (chat-role row)",
	"chat.think":    "context_backends.model_map params.think (chat role)",

	"chat_fallback.host":     "context_backends.base_url (lower-priority chat-role row)",
	"chat_fallback.api_key":  "context_backends.api_key_ref (F2 secret, lower-priority chat-role row)",
	"chat_fallback.protocol": "context_backends.protocol (lower-priority chat-role row)",
	"chat_fallback.timeout":  "context_backends.timeouts (chat role, seconds)",

	"embed.host":     "context_backends.base_url (embed-role row)",
	"embed.api_key":  "context_backends.api_key_ref (F2 secret, embed-role row)",
	"embed.protocol": "context_backends.protocol (embed-role row)",
	"embed.model":    "context_backends.model_map (embed role)",
	"embed.num_ctx":  "context_backends.num_ctx (embed-role row)",

	"dream.host":     "context_backends.base_url (dream-role row)",
	"dream.api_key":  "context_backends.api_key_ref (F2 secret, dream-role row)",
	"dream.protocol": "context_backends.protocol (dream-role row)",
	"dream.model":    "context_backends.model_map (dream role)",
	"dream.num_ctx":  "context_backends.num_ctx (dream-role row)",
	"dream.think":    "context_backends.model_map params.think (dream role)",

	"dream_embed.host":     "context_backends.base_url (dream-embed-role row)",
	"dream_embed.api_key":  "context_backends.api_key_ref (F2 secret, dream-embed-role row)",
	"dream_embed.protocol": "context_backends.protocol (dream-embed-role row)",
	"dream_embed.model":    "context_backends.model_map (dream-embed role)",
	"dream_embed.num_ctx":  "context_backends.num_ctx (dream-embed-role row)",

	"rerank.host":    "context_backends.base_url (rerank-role row)",
	"rerank.api_key": "context_backends.api_key_ref (F2 secret, rerank-role row)",
	"rerank.model":   "context_backends.model_map (rerank role)",
}

// RetiredEnvNames returns the env var names of the retired keys, sorted — the
// single name source for the boot WARN over still-set legacy vars and for the
// tombstone tripwire. Sorted rather than registry-ordered because the map has no
// order to inherit and a sweep that logs in a stable order is diffable.
func RetiredEnvNames() []string {
	out := make([]string, 0, len(retiredSettingKeys))
	for key := range retiredSettingKeys {
		out = append(out, retiredEnvName(key))
	}
	sort.Strings(out)
	return out
}

// RetiredKeyNames returns the canonical settings keys of the retirement,
// sorted — the name source of the boot-time row-shadow sweep (A06-A1,
// design/06 §3.4 #2), which looks for context_settings rows on exactly these
// keys across every scope. RetiredEnvNames() cannot serve that sweep: the
// context_settings rows are keyed by the canonical key, not by the env name,
// and deriving one from the other outside this file would plant the second
// transcript the whole file exists to prevent. Same sorted-for-diffability
// contract as RetiredEnvNames().
func RetiredKeyNames() []string {
	out := make([]string, 0, len(retiredSettingKeys))
	for key := range retiredSettingKeys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// retiredEnvName derives the env var of a retired key: "CTX_" + the upper-cased
// key with '.' replaced by '_'. The derivation is mechanical for all 29 keys —
// retired_test.go pins it against the live registry entry for as long as the
// key has one, and against the ABSENCE of the name from EnvVars() once its
// wave cut it, so a hand-written second list can never drift in from either
// side of the cut. Every vintage shares it, so no two lists can end up
// spelling the same name two ways.
func retiredEnvName(key string) string {
	return "CTX_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// retiredKeysWithoutSuccessor is the SECOND retirement vintage, and it is its
// own map for one reason: the VALUE of retiredSettingKeys is a move target
// ("the pool location that owns the value now"), and these keys have none.
// Their value does not move, it stops existing — the mechanism had already
// decided them in code. The value here is therefore the Ist statement, the
// sentence that answers the operator's "and where did it go?" with "nowhere,
// and here is why nothing changes".
//
// Folding them into the first map would cost two things at once. The single
// claim that map makes would become untrue, and the 29 in retireddocs_test.go
// — the first vintage's pin — would move, which is exactly the vintage
// confusion the separation exists against. FOUR vintages now, four behaviours:
// the gaming keys (never env-sourced, no sweep at all), the backend tuple (29
// names, suffix-filtered env sweep, Migration 133), this one (env sweep
// without a suffix filter, own release v5.16.0, Migration 152) and V3 below
// (same shape as this one, own release v5.17.0, own delete migration).
// retired_test.go pins that no name lies in two of them.
//
// A NEW VINTAGE RATHER THAN A THIRD ENTRY HERE, and the reason is mechanical:
// retiredv2migration_test.go binds this map to the ARRAY[…] of Migration 152
// by SET EQUALITY, and 152 is landed, applied and frozen by checksum. A key
// added here would turn that pin red with no repair left — the migration
// cannot be edited, so the only honest home for a later retirement is its own
// map, its own release and its own migration (K19).
//
// Same 404 on the wire as every other retirement (E13): a retired key answers
// through the ordinary unknownKey path, with nothing that separates it from a
// typo. The list is DATA for the boot advisories and for the migration that
// deletes the rows, never an API surface.
var retiredKeysWithoutSuccessor = map[string]string{
	"distill.local_only": "no successor — the distill call sets LocalOnly FIXED true in code " +
		"(internal/events/distill_extract.go, distillCall), independently of this key; it never lowered it",
	"root_map.label_budget": "no successor — the cap never had a subject: internal/rootmap imports " +
		"no llm package and the field had no non-test reader, so label production never reached the " +
		"read path the budget was declared for",
}

// RetiredV2EnvNames returns the env var names of the second vintage, sorted —
// the name source of the V2 boot env sweep, with the same
// sorted-for-diffability contract as RetiredEnvNames().
//
// The V2 sweep applies NO suffix filter to this list. The first vintage's
// three suffixes (_HOST/_API_KEY/_MODEL) select the value-bearing half of a
// topology tuple whose other half arrives scaffolded non-empty on a whole
// cohort; that partition is a statement about the backend tuple and says
// nothing about these keys, every one of which carries an operator's value.
func RetiredV2EnvNames() []string {
	out := make([]string, 0, len(retiredKeysWithoutSuccessor))
	for key := range retiredKeysWithoutSuccessor {
		out = append(out, retiredEnvName(key))
	}
	sort.Strings(out)
	return out
}

// RetiredV2KeyNames returns the canonical settings keys of the second vintage,
// sorted — the name source of the second boot row-shadow sweep, and the SET
// the delete migration of this vintage binds its ARRAY[…] against (design/05
// §3: a key missing from that array leaves its rows behind on every foreign
// installation, invisible after the registry cut and live configuration again
// the day someone re-registers the name).
//
// Separate from RetiredV2EnvNames() for the reason RetiredKeyNames() is
// separate from RetiredEnvNames(): context_settings rows are keyed by the
// canonical key, and deriving one from the other outside this file would plant
// the second transcript the whole file exists to prevent.
func RetiredV2KeyNames() []string {
	out := make([]string, 0, len(retiredKeysWithoutSuccessor))
	for key := range retiredKeysWithoutSuccessor {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// retiredKeysWithoutSuccessorV3 is the THIRD retirement vintage, and it is the
// second one whose values do not move anywhere: same VALUE contract as
// retiredKeysWithoutSuccessor above — the Ist statement that answers the
// operator's "and where did it go?" with "nowhere, and here is why nothing
// changes" — but its own map, its own release and its own delete migration,
// because Migration 152 is landed and frozen and its set-equality pin against
// the V2 map has no repair path (see the note there).
//
// ONE SUBJECT DIED AND TOOK ALL THREE KEYS WITH IT. The distiller arm used to
// carry a second source: a reader of a FOREIGN, read-only SQLite state file of
// an agent runtime (internal/hermesstate) behind a distillsource adapter
// (internal/distillsource/hermesadapter). Both packages are gone in v5.17.0,
// so the path these three keys configured — which file to open, what to call
// it in the journal, how long its sessions had to be quiet — has no code left
// to reach. The arm keeps its remaining source, the ctx-checkpoint reader over
// this store's own corpus, and that source has always had its own keys.
//
// distill.ctx_source_label AND distill.ctx_quiet_for ARE NOT SUCCESSORS. They
// are the ctx-checkpoint source's OWN keys, minted with that source in A02-4
// and live ever since — an operator who copies a value from a retired key into
// one of them is not migrating a setting, he is overwriting a different
// source's configuration with a number that was measured against a different
// artifact. The similar spelling is the whole reason this paragraph exists.
//
// Same 404 on the wire as every other retirement (E13): a retired key answers
// through the ordinary unknownKey path, with nothing that separates it from a
// typo. The list is DATA for the boot advisories and for the migration that
// deletes the rows, never an API surface.
var retiredKeysWithoutSuccessorV3 = map[string]string{
	"distill.source_label": "no successor — the source it named is gone: the reader of the foreign " +
		"agent state file (internal/hermesstate) and its distillsource adapter fell in v5.17.0. " +
		"distill.ctx_source_label is NOT this key under a new name; it is the ctx-checkpoint " +
		"source's own label, live since A02-4, and copying a value into it renames THAT source",
	"distill.session_quiet_for": "no successor — the gate measured the youngest live row of a " +
		"session in the foreign state file, and there is no such file to read any more. " +
		"distill.ctx_quiet_for is NOT its replacement: it is the ctx-checkpoint source's own gate " +
		"over checkpoint ages, with its own measured default (30 min, decision EA-5)",
	"distill.source_path": "no successor — the path pointed at the foreign agent state file the arm " +
		"opened per tick, and nothing opens a file any more: the remaining source reads " +
		"context_blocks through the pool the daemon already holds",
}

// RetiredV3EnvNames returns the env var names of the third vintage, sorted —
// the name source of the V3 boot env sweep, with the same
// sorted-for-diffability contract as RetiredEnvNames().
//
// NO SUFFIX FILTER, for the reason RetiredV2EnvNames() gives: the first
// vintage's three suffixes select the value-bearing half of a topology tuple
// whose other half arrives scaffolded non-empty on a whole cohort, and that
// partition says nothing about these three — every one of them carries a value
// an operator chose.
func RetiredV3EnvNames() []string {
	out := make([]string, 0, len(retiredKeysWithoutSuccessorV3))
	for key := range retiredKeysWithoutSuccessorV3 {
		out = append(out, retiredEnvName(key))
	}
	sort.Strings(out)
	return out
}

// RetiredV3KeyNames returns the canonical settings keys of the third vintage,
// sorted — the name source of the V3 boot row-shadow sweep, and the SET the
// delete migration of this vintage binds its ARRAY[…] against (same contract
// as RetiredV2KeyNames(): a key missing from that array leaves its rows behind
// on every foreign installation, invisible after the registry cut and live
// configuration again the day someone re-registers the name).
//
// Separate from RetiredV3EnvNames() for the reason RetiredKeyNames() is
// separate from RetiredEnvNames(): context_settings rows are keyed by the
// canonical key, and deriving one from the other outside this file would plant
// the second transcript the whole file exists to prevent.
func RetiredV3KeyNames() []string {
	out := make([]string, 0, len(retiredKeysWithoutSuccessorV3))
	for key := range retiredKeysWithoutSuccessorV3 {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
