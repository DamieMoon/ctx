package main

// A06-A1 / K4 (design/06 §3.4 + §3.5, design/01 §4 + §7 W9) — the ENV half of
// the retirement's boot sweep, and the ingredient list it is built from.
//
// The ROW half needs a database and lives in retiredsources_integration_test.go.
// It is untouched by the cut: it keys off config.RetiredKeyNames(), a static
// list, so it keeps naming leftover context_settings rows in every scope.
//
// The ENV half had a predecessor and an interregnum. warnRetiredEnvVarsBoot
// chose its per-key wording from c.sources ("this var IS the effective source"
// vs. "a settings row already shadows it"), so it could only ever speak about
// keys the loader still knew; β8 cut the last six of the 29 out of the registry
// and the sweep died with its subject, exactly as design/06 §4 Phase A #1 had
// written before the first key moved: "im Schnitt wird er durch den Tombstone
// 3.5 ersetzt, weil c.sources die Keys dann nicht mehr kennt." Between β8 and
// β13 nothing spoke about a still-set CTX_CHAT_HOST at all — a gap this file
// pinned as an open debt rather than letting the silence pass for safety.
//
// β13 discharges it. The tombstone asks the environment directly instead of the
// loader, so it can speak about names no registry knows any more, and the pin
// below turned from a reminder into the sweep's precondition: the cut must stay
// complete, and the ingredient list must stay whole, or the sweep silently
// stops covering part of the retirement.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// deprecationLines counts the log lines carrying exactly this deprecation
// label. A plain strings.Contains cannot do it once there are two vintages:
// "retired_env" is a PREFIX of "retired_env_v2", so a substring test for the
// first vintage's label passes on a line that only ever carried the second
// one's — and the negative arms that keep the two windows apart would then be
// permanently, invisibly true. `\b` does not match between "env" and "_v2"
// because '_' is a word character, which is what makes the label exact here.
func deprecationLines(out, label string) int {
	return len(regexp.MustCompile(`deprecation=`+regexp.QuoteMeta(label)+`\b`).FindAllString(out, -1))
}

// needle is the value probe of the name-only rule. Distinctive enough that a
// substring search over a whole log buffer cannot hit it by accident, and
// shaped like the thing that must never be logged: a provider api key.
const needle = "sk-live-NEEDLE-must-never-be-logged-7f3a91"

// TestRetiredEnvSweepPreconditions pins the two facts the tombstone sweep is
// built on. Neither is about the sweep's behaviour — that is the test below —
// and both would break it silently rather than loudly.
//
//  1. The cut is COMPLETE: no retired key is registered any more. This is WHY
//     the env half had to be rebuilt on a static list. If it ever fails, a
//     retired name is back in the registry and the far bigger problem is that
//     every stale row on it became effective configuration again
//     (config/retired.go).
//  2. The INGREDIENT is whole: config.RetiredEnvNames() still returns all 29
//     names, sorted and duplicate-free, and the value-bearing partition the
//     tripwire sweeps is exactly 6 hosts + 6 api keys + 5 models = 17 of them.
//     The partition is asserted by CLASS COUNT, not by a transcript of
//     seventeen strings: a second transcript is the thing config/retired.go
//     exists to prevent, but a suffix rule that started matching a fourth class
//     — or stopped matching one — would otherwise change the sweep's reach
//     without changing a single visible name.
func TestRetiredEnvSweepPreconditions(t *testing.T) {
	for _, key := range config.RetiredKeyNames() {
		if info, registered := config.KeyByName(key); registered {
			t.Errorf("%s is registered again (env %q) — a retired name back in the registry revives every "+
				"stale row on it; the β8 cut is not complete", key, info.EnvVar)
		}
	}

	names := config.RetiredEnvNames()
	if len(names) != 29 {
		t.Errorf("RetiredEnvNames() returned %d names, want 29 — the tombstone tripwire sweeps a subset of "+
			"exactly this list", len(names))
	}
	seen := map[string]bool{}
	for _, name := range names {
		if !strings.HasPrefix(name, "CTX_") {
			t.Errorf("RetiredEnvNames() carries %q, which is not a CTX_ env var name", name)
		}
		if seen[name] {
			t.Errorf("RetiredEnvNames() lists %s twice", name)
		}
		seen[name] = true
	}

	byClass := map[string]int{}
	for _, name := range retiredEnvTripwireNames() {
		switch {
		case strings.HasSuffix(name, "_API_KEY"):
			byClass["api_key"]++
		case strings.HasSuffix(name, "_HOST"):
			byClass["host"]++
		case strings.HasSuffix(name, "_MODEL"):
			byClass["model"]++
		default:
			t.Errorf("tripwire sweeps %q, which belongs to none of the three value-bearing classes", name)
		}
	}
	for class, want := range map[string]int{"host": 6, "api_key": 6, "model": 5} {
		if byClass[class] != want {
			t.Errorf("tripwire sweeps %d %s vars, want %d (design/01 §4 W9: 6 hosts, 6 api_keys, 5 models)",
				byClass[class], class, want)
		}
	}
	if got := len(retiredEnvTripwireNames()); got != 17 {
		t.Errorf("tripwire sweeps %d of the 29 names, want 17 — the twelve value-less ones "+
			"(protocols, timeout, num_ctx, think) must stay out", got)
	}
}

// TestWarnRetiredEnvVarsBoot is the K4 gate (design/01 §7 W9): one WARN naming
// the var and the way out on a set, non-empty, value-bearing legacy var — and
// silence in every one of the four cases that would otherwise make this sweep
// noise instead of signal.
//
// The negative arms carry the weight. This tripwire runs on a cohort that did
// NOT update its compose file, which is precisely the cohort whose compose file
// materialises all 29 vars as `${VAR:-}` — a sweep without the value filter
// would print up to 29 lines on every boot of every such installation, and the
// seventeen lines that can mean a dead host would drown in them.
func TestWarnRetiredEnvVarsBoot(t *testing.T) {
	t.Run("set non-empty legacy var warns by name with the way out", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv("CTX_CHAT_HOST", "x")

		warnRetiredEnvVarsBoot()

		out := buf.String()
		if !strings.Contains(out, "level=WARN") {
			t.Errorf("log = %q, want a WARN — a silently ignored backend host is not an INFO", out)
		}
		if !strings.Contains(out, "CTX_CHAT_HOST") {
			t.Errorf("log = %q, want the var NAME — the operator has to know which line of his .env is dead", out)
		}
		// Without a next step this is an alarm, not an instruction: the value
		// moved somewhere, and the line has to say where.
		if !strings.Contains(out, "ctx backends") {
			t.Errorf("log = %q, want the 'ctx backends' pointer to the new home of the value", out)
		}
		if !strings.Contains(out, retiredMajor) {
			t.Errorf("log = %q, want the release the key disappeared in (%s)", out, retiredMajor)
		}
		if n := deprecationLines(out, deprecationRetiredEnv); n != 1 {
			t.Errorf("log = %q, want the deprecation label — it is how the whole window greps out of a JSON boot log", out)
		}
		if n := strings.Count(out, "level=WARN"); n != 1 {
			t.Errorf("log = %q, want exactly 1 WARN for 1 set var, got %d", out, n)
		}
	})

	// Negative 1: nothing set at all. A tripwire that fires on a clean
	// environment reports its own existence, not a problem.
	t.Run("negative 1: no legacy var set is silent", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)

		warnRetiredEnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q on a clean environment, want silence", out)
		}
	})

	// Negative 2: the compose-scaffold case, and the reason the filter is on
	// the VALUE rather than on existence. `${CTX_CHAT_HOST:-}` in an unmodified
	// v4 compose file reaches the process as set-and-empty; the loader treats
	// empty env as unset (load.go:296) and so must this.
	t.Run("negative 2: set-but-empty is not set", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		for _, name := range retiredEnvTripwireNames() {
			t.Setenv(name, "")
		}

		warnRetiredEnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q with all 17 vars set-but-empty, want silence — this is the state every "+
				"unmodified v4 compose file produces", out)
		}
	})

	// Negative 3: the two scaffold DEFAULTS. Same cohort, but these two arrive
	// non-empty because their compose declaration shipped a default value, so
	// the value filter alone would not stop them.
	t.Run("negative 3: scaffold default values are exempt", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		for name, def := range retiredEnvScaffoldDefaults {
			t.Setenv(name, def)
		}

		warnRetiredEnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q on the untouched rerank scaffold defaults, want silence", out)
		}
	})

	// The exemption is value-scoped, not name-scoped — the control that makes
	// negative 3 mean something. An operator who pointed rerank at his own host
	// made a real choice, and that choice is exactly what dies silently.
	t.Run("a scaffold-default var with a different value still warns", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv("CTX_RERANK_HOST", "http://rerank.internal:9000")

		warnRetiredEnvVarsBoot()

		if out := buf.String(); !strings.Contains(out, "CTX_RERANK_HOST") {
			t.Errorf("log = %q, want the WARN — only the scaffold VALUE is exempt, not the name", out)
		}
	})

	// The twelve value-less names, as a class. Their compose defaults are
	// hard-wired non-empty, so they pass the value filter and are stopped only
	// by the partition.
	t.Run("value-less legacy vars stay out of the sweep", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		swept := map[string]bool{}
		for _, name := range retiredEnvTripwireNames() {
			swept[name] = true
		}
		var quiet []string
		for _, name := range config.RetiredEnvNames() {
			if !swept[name] {
				quiet = append(quiet, name)
				t.Setenv(name, "some-non-empty-value")
			}
		}
		if len(quiet) != 12 {
			t.Fatalf("expected 12 value-less names outside the sweep, got %d (%v)", len(quiet), quiet)
		}

		warnRetiredEnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q, want silence — protocols/timeout/num_ctx/think carry no topology and their "+
				"compose scaffold defaults are non-empty on every unmodified v4 install", out)
		}
	})

	// Negative 4, the needle: NO value ever reaches the log. Six of the
	// seventeen names are api_key vars and a boot log travels into aggregators
	// and support bundles, so a sweep that echoed what it found would turn a
	// deprecation notice into a credential leak.
	t.Run("negative 4: no value is ever logged (needle)", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv("CTX_CHAT_API_KEY", needle)

		warnRetiredEnvVarsBoot()

		out := buf.String()
		if !strings.Contains(out, "CTX_CHAT_API_KEY") {
			t.Fatalf("log = %q, want the var name — without the positive half the needle scan proves nothing", out)
		}
		if strings.Contains(out, needle) {
			t.Errorf("the api key VALUE reached the boot log:\n%s", out)
		}
	})

	// Every value-bearing name gets a voice: one line each, none swallowed.
	t.Run("all 17 value-bearing vars are reported", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		names := retiredEnvTripwireNames()
		for _, name := range names {
			t.Setenv(name, "non-default-value")
		}

		warnRetiredEnvVarsBoot()

		out := buf.String()
		for _, name := range names {
			if !strings.Contains(out, "env="+name) {
				t.Errorf("log = %q, missing %s", out, name)
			}
		}
		if n := strings.Count(out, "level=WARN"); n != len(names) {
			t.Errorf("got %d WARN lines for %d set vars", n, len(names))
		}
	})
}

// TestWarnRetiredV2EnvVarsBoot is the same gate for the SECOND retirement
// vintage (config/retired.go, retiredKeysWithoutSuccessor), and it is the
// reason that vintage is a tombstone rather than a silent removal: without it,
// a deployment that still exports the variable boots in perfect silence,
// because the loader is registry-driven and the registry has never heard of
// the key.
//
// The differences from the V1 gate above are the differences of the sweep:
//
//   - NO SUFFIX PARTITION. Every name of this vintage is swept, so the
//     positive arm can use any of them and the "value-less names stay out"
//     arm has no subject here.
//   - OWN RELEASE in the text (retiredV2Release), and NOT retiredMajor — a
//     line naming v5.0.0 would send the operator to the backend-tuple runbook.
//   - NO SUCCESSOR to name. The V1 line points at `ctx backends`; this one has
//     nothing to point at and must say so instead of implying a move.
//
// The value filter and the name-only rule are pinned identically, because they
// are the two properties that decide whether this sweep is signal or noise on
// an installation that never touched the variable.
func TestWarnRetiredV2EnvVarsBoot(t *testing.T) {
	v2 := config.RetiredV2EnvNames()
	if len(v2) == 0 {
		t.Fatal("RetiredV2EnvNames() is empty — the second vintage carries no key, so this sweep has no subject")
	}
	probe := v2[0]

	t.Run("set non-empty retired var warns by name with the way out", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv(probe, "false")

		warnRetiredV2EnvVarsBoot()

		out := buf.String()
		if !strings.Contains(out, "level=WARN") {
			t.Errorf("log = %q, want a WARN — a silently ignored setting is not an INFO", out)
		}
		if !strings.Contains(out, probe) {
			t.Errorf("log = %q, want the var NAME — the operator has to know which line of his .env is dead", out)
		}
		if !strings.Contains(out, retiredV2Release) {
			t.Errorf("log = %q, want the release the key disappeared in (%s)", out, retiredV2Release)
		}
		// The label is how the whole window greps out of a JSON boot log, and
		// it is the vintage's own: a shared one would sum two windows.
		if n := deprecationLines(out, deprecationRetiredEnvV2); n != 1 {
			t.Errorf("log = %q, want 1 line with the second vintage's deprecation label, got %d", out, n)
		}
		if n := deprecationLines(out, deprecationRetiredEnv); n != 0 {
			t.Errorf("log = %q, carries the FIRST vintage's label — the two windows must stay apart", out)
		}
		// v5.0.0 belongs to the backend tuple. A line of this vintage naming
		// it sends an operator to a runbook section about a pool and a
		// migration that have nothing to do with his key.
		if strings.Contains(out, retiredMajor) {
			t.Errorf("log = %q, must not name %s — that is the first vintage's release", out, retiredMajor)
		}
		if strings.Contains(out, "ctx backends") {
			t.Errorf("log = %q, must not point at the backend pool — this key has no successor", out)
		}
		if n := strings.Count(out, "level=WARN"); n != 1 {
			t.Errorf("log = %q, want exactly 1 WARN for 1 set var, got %d", out, n)
		}
	})

	// The value filter, byte for byte the V1 rule: a compose file that
	// materialises the name as `${VAR:-}` delivers it set and empty, and the
	// loader treats empty env as unset (load.go). A sweep that warned here
	// would fire on installations that never chose anything.
	t.Run("negative: set-but-empty is not set", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		for _, name := range v2 {
			t.Setenv(name, "")
		}

		warnRetiredV2EnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q with every V2 var set-but-empty, want silence", out)
		}
	})

	t.Run("negative: nothing set is silent", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)

		warnRetiredV2EnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q on a clean environment, want silence", out)
		}
	})

	// The V1 sweep must not speak about a V2 name and vice versa. This is the
	// runtime half of the list separation retired_test.go pins statically: two
	// sweeps over one name would log it twice, with two different releases.
	//
	// Two subtests rather than two halves of one, because resetAllEnv only
	// neutralises config.EnvVars() — the LIVE registry names. A retired name
	// is by definition not among them, so a t.Setenv on one survives inside
	// the subtest that made it and only the subtest boundary takes it back.
	t.Run("the V1 sweep does not speak about a V2 name", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv(probe, "false")

		warnRetiredEnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q — the V1 sweep spoke about a V2 name", out)
		}
	})

	t.Run("the V2 sweep does not speak about a V1 name", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv("CTX_CHAT_HOST", "x")

		warnRetiredV2EnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q — the V2 sweep spoke about a V1 name", out)
		}
	})

	// Name-only, like the whole sweep. The V2 names are not secret-class
	// today, but the rule is the sweep's and not the key's: the next member of
	// this vintage inherits it, and a boot log travels into aggregators.
	t.Run("no value is ever logged (needle)", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv(probe, needle)

		warnRetiredV2EnvVarsBoot()

		out := buf.String()
		if !strings.Contains(out, probe) {
			t.Fatalf("log = %q, want the var name — without the positive half the needle scan proves nothing", out)
		}
		if strings.Contains(out, needle) {
			t.Errorf("the value reached the boot log:\n%s", out)
		}
	})

	// Every name of the vintage gets a voice: one line each, none swallowed by
	// a filter that was written for the other vintage's shape.
	t.Run("every V2 var is reported", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		for _, name := range v2 {
			t.Setenv(name, "non-default-value")
		}

		warnRetiredV2EnvVarsBoot()

		out := buf.String()
		for _, name := range v2 {
			if !strings.Contains(out, "env="+name) {
				t.Errorf("log = %q, missing %s", out, name)
			}
		}
		if n := strings.Count(out, "level=WARN"); n != len(v2) {
			t.Errorf("got %d WARN lines for %d set vars", n, len(v2))
		}
	})

	// The scaffold channel exists before it is needed. It is empty today
	// because no V2 name is declared in the tracked compose file; the arm
	// pins that an entry, once added, is VALUE-scoped — the shape the next
	// member of this vintage needs (a declared `${VAR:-0}`), and the shape
	// that keeps a real operator choice on the same name warning.
	t.Run("the scaffold exemption is value-scoped", func(t *testing.T) {
		if len(retiredV2EnvScaffoldDefaults) == 0 {
			resetAllEnv(t)
			buf := captureBootLog(t)
			t.Setenv(probe, "any-value")

			warnRetiredV2EnvVarsBoot()

			if !strings.Contains(buf.String(), probe) {
				t.Errorf("log = %q, want the WARN — with no exemption every non-empty value warns", buf.String())
			}
			return
		}
		for name, def := range retiredV2EnvScaffoldDefaults {
			resetAllEnv(t)
			buf := captureBootLog(t)
			t.Setenv(name, def)
			warnRetiredV2EnvVarsBoot()
			if out := buf.String(); out != "" {
				t.Errorf("log = %q on the untouched scaffold default of %s, want silence", out, name)
			}

			resetAllEnv(t)
			buf = captureBootLog(t)
			t.Setenv(name, def+"-changed")
			warnRetiredV2EnvVarsBoot()
			if out := buf.String(); !strings.Contains(out, name) {
				t.Errorf("log = %q, want the WARN — only the scaffold VALUE is exempt, not the name", out)
			}
		}
	})
}

// TestWarnRetiredV3EnvVarsBoot is the env gate of the THIRD retirement vintage
// (config/retired.go, retiredKeysWithoutSuccessorV3) and the twin of the V2
// gate above. It closes the same silence: the loader is registry-driven, so a
// deployment that still exports one of these variables boots without a word,
// because the registry has never heard of the key.
//
// Two properties are this vintage's own and are asserted here rather than
// inherited:
//
//   - OWN RELEASE in the text (retiredV3Release). A line naming v5.0.0 sends
//     the operator to the backend-tuple runbook; one naming v5.16.0 sends him
//     to a window his key was never in.
//   - THE SUBJECT IS NAMED AS GONE, not merely the key. These three keys
//     configured a source that no longer exists, so a line that only said "no
//     successor" would leave an operator hunting for a replacement key that
//     was never minted.
//
// The value filter and the name-only rule are pinned identically to both
// siblings, because they are what decides whether the sweep is signal or noise
// on an installation that never touched the variable.
func TestWarnRetiredV3EnvVarsBoot(t *testing.T) {
	v3 := config.RetiredV3EnvNames()
	if len(v3) == 0 {
		t.Fatal("RetiredV3EnvNames() is empty — the third vintage carries no key, so this sweep has no subject")
	}
	probe := v3[0]

	t.Run("set non-empty retired var warns by name with the way out", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv(probe, "600")

		warnRetiredV3EnvVarsBoot()

		out := buf.String()
		if !strings.Contains(out, "level=WARN") {
			t.Errorf("log = %q, want a WARN — a silently ignored setting is not an INFO", out)
		}
		if !strings.Contains(out, probe) {
			t.Errorf("log = %q, want the var NAME — the operator has to know which line of his .env is dead", out)
		}
		if !strings.Contains(out, retiredV3Release) {
			t.Errorf("log = %q, want the release the key disappeared in (%s)", out, retiredV3Release)
		}
		if n := deprecationLines(out, deprecationRetiredEnvV3); n != 1 {
			t.Errorf("log = %q, want 1 line with the third vintage's deprecation label, got %d", out, n)
		}
		for _, foreign := range []struct{ label, why string }{
			{deprecationRetiredEnv, "the FIRST vintage's label"},
			{deprecationRetiredEnvV2, "the SECOND vintage's label"},
		} {
			if n := deprecationLines(out, foreign.label); n != 0 {
				t.Errorf("log = %q, carries %s — the windows must stay apart", out, foreign.why)
			}
		}
		// Both older releases belong to other windows. A line of this vintage
		// naming either sends an operator to a runbook section about keys he
		// never had.
		for _, wrong := range []string{retiredMajor, retiredV2Release} {
			if strings.Contains(out, wrong) {
				t.Errorf("log = %q, must not name %s — that is another vintage's release", out, wrong)
			}
		}
		if strings.Contains(out, "ctx backends") {
			t.Errorf("log = %q, must not point at the backend pool — this key has no successor", out)
		}
		if n := strings.Count(out, "level=WARN"); n != 1 {
			t.Errorf("log = %q, want exactly 1 WARN for 1 set var, got %d", out, n)
		}
	})

	// The value filter, byte for byte the rule of both siblings: the tracked
	// compose file materialises every one of these names as `${VAR:-}`, so on
	// an untouched installation they all arrive set and empty, and the loader
	// treats empty env as unset (load.go). A sweep that warned here would fire
	// on the whole deployed cohort.
	t.Run("negative: set-but-empty is not set", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		for _, name := range v3 {
			t.Setenv(name, "")
		}

		warnRetiredV3EnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q with every V3 var set-but-empty, want silence", out)
		}
	})

	t.Run("negative: nothing set is silent", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)

		warnRetiredV3EnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q on a clean environment, want silence", out)
		}
	})

	// No sweep speaks about another vintage's name. This is the runtime half
	// of the list separation retired_test.go pins statically: two sweeps over
	// one name would log it twice, with two different releases.
	//
	// Separate subtests rather than halves of one, because resetAllEnv only
	// neutralises config.EnvVars() — the LIVE registry names. A retired name
	// is by definition not among them, so a t.Setenv on one survives inside
	// the subtest that made it and only the subtest boundary takes it back.
	t.Run("the older sweeps do not speak about a V3 name", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv(probe, "600")

		warnRetiredEnvVarsBoot()
		warnRetiredV2EnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q — an older sweep spoke about a V3 name", out)
		}
	})

	t.Run("the V3 sweep does not speak about an older name", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv("CTX_CHAT_HOST", "x")
		t.Setenv("CTX_DISTILL_LOCAL_ONLY", "true")

		warnRetiredV3EnvVarsBoot()

		if out := buf.String(); out != "" {
			t.Errorf("log = %q — the V3 sweep spoke about an older vintage's name", out)
		}
	})

	// Name-only, like every sibling. These names are not secret-class, but the
	// rule is the sweep's and not the key's — a path is deployment topology,
	// and a boot log travels into aggregators.
	t.Run("no value is ever logged (needle)", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		t.Setenv(probe, needle)

		warnRetiredV3EnvVarsBoot()

		out := buf.String()
		if !strings.Contains(out, probe) {
			t.Fatalf("log = %q, want the var name — without the positive half the needle scan proves nothing", out)
		}
		if strings.Contains(out, needle) {
			t.Errorf("the value reached the boot log:\n%s", out)
		}
	})

	// Every name of the vintage gets a voice: one line each, none swallowed by
	// a filter written for another vintage's shape.
	t.Run("every V3 var is reported", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)
		for _, name := range v3 {
			t.Setenv(name, "non-default-value")
		}

		warnRetiredV3EnvVarsBoot()

		out := buf.String()
		for _, name := range v3 {
			if !strings.Contains(out, "env="+name) {
				t.Errorf("log = %q, missing %s", out, name)
			}
		}
		if n := strings.Count(out, "level=WARN"); n != len(v3) {
			t.Errorf("got %d WARN lines for %d set vars", n, len(v3))
		}
	})

	// This vintage has NO scaffold exemption, and the arm states it as a
	// property rather than leaving it as an absence: the three names entered
	// the tracked compose file only in `${NAME:-}` form, so no installation
	// ever receives one of them with a value nobody chose. Every non-empty
	// value is therefore an operator's own line and warns.
	t.Run("every non-empty value warns — no scaffold exemption exists", func(t *testing.T) {
		for _, name := range v3 {
			resetAllEnv(t)
			buf := captureBootLog(t)
			t.Setenv(name, "any-value")

			warnRetiredV3EnvVarsBoot()

			if !strings.Contains(buf.String(), name) {
				t.Errorf("log = %q, want the WARN on %s — with no exemption every non-empty value warns", buf.String(), name)
			}
		}
	})
}

// TestBothRetirementSweepsAreWiredAtBoot pins the CALL SITE of all six boot
// sweeps, which is the one property every behaviour test above misses: they
// call the functions directly, so a sweep deleted from the boot block keeps
// them all green while the daemon goes silent. A list with a mechanism nobody
// runs is a list without a mechanism — the exact failure mode the successorless
// vintages were given their own consumers to avoid.
//
// Source-level rather than behavioural because the alternative is booting a
// daemon: main() takes a pool, a listener and a full config before it reaches
// this block. The AST walk is precise where a grep would not be — it counts
// CALLS, so the names in comments and doc blocks around them do not count, and
// it reads the non-test files of this package only.
func TestBothRetirementSweepsAreWiredAtBoot(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/ctxd: %v", err)
	}

	want := map[string]string{
		"warnRetiredEnvVarsBoot":       "",
		"warnRetiredSettingRowsBoot":   "",
		"warnRetiredV2EnvVarsBoot":     "",
		"warnRetiredV2SettingRowsBoot": "",
		"warnRetiredV3EnvVarsBoot":     "",
		"warnRetiredV3SettingRowsBoot": "",
	}
	count := map[string]int{}
	fset := token.NewFileSet()
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		parsed++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			enclosing := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				if _, tracked := want[id.Name]; !tracked {
					return true
				}
				count[id.Name]++
				want[id.Name] = enclosing
				return true
			})
		}
	}
	// Without a file the walk below is vacuously green, which would make this
	// gate report its own emptiness as a pass.
	if parsed == 0 {
		t.Fatal("no non-test .go file parsed in cmd/ctxd — the scan proves nothing")
	}

	for name := range want {
		if count[name] != 1 {
			t.Errorf("%s is called %d times in cmd/ctxd (non-test), want exactly 1 — a sweep that is not "+
				"called at boot reports nothing, and a sweep called twice reports every row twice", name, count[name])
		}
	}
	// One block, one place in the log: an operator greps `deprecation=` once,
	// and two halves of one window split across the boot would read as
	// unrelated problems. Same argument the V1 pair's call site carries.
	if t.Failed() {
		return
	}
	host := want["warnRetiredEnvVarsBoot"]
	for name, in := range want {
		if in != host {
			t.Errorf("%s is called in %s, the others in %s — every sweep belongs in one boot block", name, in, host)
		}
	}
}

// TestRetiredEnvNamesStayOutOfTheLiveEnvSurface is the inverse statement, at the
// boot layer rather than the registry layer: not one of the 29 names may be
// readable as configuration any more.
//
// config/retired_test.go pins the same thing against EnvVars(). This one is
// worth having next to it because it is the layer an operator's .env actually
// meets: it sets every retired name to a distinctive value, boots the env
// config, and requires that none of the values reaches the rendered snapshot.
// A key that lost its struct field but kept an env source — or a future key that
// reintroduced one of the names under a different key — shows up here as a value
// in the render, not as a count mismatch somewhere else.
//
// The scan runs over Redacted(SurfaceBootDump), NOT over BootDumpArgs: the boot
// record renders a CURATED SUBSET of the groups (dumpGroupOrder, six of them),
// so a value landing in any other group would pass a BootDumpArgs scan
// unnoticed. Redacted produces every registry group, which is what makes the
// negative statement worth making. Verified by construction: the control below
// sets a LIVE env key to the same marker and requires it to BE in the render —
// without it, "the marker is absent" would also hold for a render that shows
// nothing at all.
func TestRetiredEnvNamesStayOutOfTheLiveEnvSurface(t *testing.T) {
	resetAllEnv(t)
	const marker = "RETIREDMARKER-must-not-be-read"
	const controlMarker = "CONTROLMARKER-must-be-read"
	for _, name := range config.RetiredEnvNames() {
		t.Setenv(name, marker)
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password-123")
	t.Setenv("CTX_DIGEST_MODE", controlMarker)

	cfg, _ := config.FromEnv()
	rendered := fmt.Sprint(cfg.Redacted(config.SurfaceBootDump))
	if !strings.Contains(rendered, controlMarker) {
		t.Fatalf("the control env var did not reach the render — the scan below would prove nothing:\n%s", rendered)
	}
	if strings.Contains(rendered, marker) {
		t.Errorf("a retired env var reached the effective config:\n%s", rendered)
	}
	for _, key := range config.RetiredKeyNames() {
		if src := cfg.Source(key); src != "" {
			t.Errorf("%s still has a source (%q) — a retired key must be unknown to the loader", key, src)
		}
	}
}
