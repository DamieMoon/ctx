package dream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/prompts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Decide mode (2026-09-20): the two dream classifiers — link evaluation and
// the recurrence confirm — can run as prefill-only decisions instead of
// JSON-generating prompts. One request per (source, candidate) pair asks for a
// single letter; the answer is read off the first token's top-logprobs
// (llm.DecideChoice). Evidence, method and the measured parity with the
// generating prompts: .project/bench-jev-2026-09-20/REPORT.md.
//
// What changes on the wire: N short requests instead of one long one, zero
// output tokens instead of ~120 (p90 244), no JSON to parse — the two failure
// classes that cost 2.4 % of dream-eval calls 16–23 s each simply do not
// exist. What does NOT change: every candidate still pays its prefill, the
// post-parse constraints (supersedes direction, candidate filter, per-type
// confidence gate, hard cap) run unchanged on the resulting links, and the
// llmlog row shape is the same — one row per wire call, metadata.decide=true.
//
// Fallback: a backend that reports no logprobs, or a first token outside the
// label set (a thinking tag, prose), is llm.IsDecideFallback — the block is
// re-evaluated ONCE through the generating path for this cycle, so a chain
// that lands on such a row (Ollama, some relays) degrades to today's
// behaviour instead of losing the verdict.

// The three accepted values of config dream.decide_mode. They live here, next
// to the resolver that reads them, for the same reason JSONModeStrict does:
// config validates against the consumer's vocabulary.
const (
	// DecideModeOff keeps both classifiers on their generating prompts —
	// today's behavior and the shipped default.
	DecideModeOff = "off"
	// DecideModeEval runs link evaluation as decisions, recurrence unchanged.
	DecideModeEval = "eval"
	// DecideModeAll runs link evaluation AND the recurrence confirm as
	// decisions.
	DecideModeAll = "all"
)

// decideEvalSystemPrompt carries the V5 relationship vocabulary verbatim (the
// type definitions are the benchmarked ones) and replaces the JSON output
// contract with a letter contract. One candidate per request: the pairwise
// framing is what makes the answer a single token.
const decideEvalSystemPrompt = `Classify the relationship between a source block and ONE candidate block.

Default: if the candidate shares meaningful content with source but doesn't clearly fit a stronger type, use topical.

Types:
- supersedes: source contains a concrete specific fact that target explicitly corrects or replaces. VERY RARE — requires the source fact to be concretely wrong/obsolete AND target to state the authoritative replacement. Thematic update, newer timestamp, or general progress is NOT supersedes.
- causal: source describes a concrete event, decision, or change whose occurrence enabled target to exist or take its current form. Source must meaningfully predate target. A decision followed by its implementation qualifies. Parallel activity or shared timeline alone does NOT.
- factual: source SPECIFIES a concrete thing (parameter, rule, config, contract, spec) that target directly implements or uses. One-way SPEC→IMPL direction required. Peer-level blocks at the same abstraction are NOT factual — they are topical.
- topical: both blocks treat the same specific topic substantively. This is the common case for genuinely related blocks that aren't in a spec→impl or cause→effect relationship.
- none: the candidate does not relate to the source in any of the above ways.

Answer with exactly one letter and nothing else: A = supersedes, B = causal, C = factual, D = topical, E = none.`

// promptDecideEval is the identity of the decide-mode link classifier.
var promptDecideEval = prompts.Register(
	"dream.decideEvalSystemPrompt", "2026-09-20", "github.com/GottZ/ctx/internal/dream")

// decideLinkLabels is the answer vocabulary in prompt order; decideLinkTypes
// maps each label to the relationship it stands for. "E" is the no-link
// option and has no relationship.
var (
	decideLinkLabels = []string{"A", "B", "C", "D", "E"}
	decideLinkTypes  = map[string]string{
		"A": "supersedes",
		"B": "causal",
		"C": "factual",
		"D": "topical",
	}
	decideNoneLabel = "E"
	// decideLinkNames is decideLinkTypes plus the no-link label, for the
	// llmlog stamp.
	decideLinkNames = map[string]string{
		"A": "supersedes", "B": "causal", "C": "factual", "D": "topical", "E": "none",
	}
)

// errDecideFallback marks a decide attempt whose answer could not be read as
// a decision (no logprobs / no label mass). evaluateRelationships catches it
// and runs the generating path once.
var errDecideFallback = errors.New("dream: decide fallback")

// decideIncapable remembers, per backend NAME, when a decide call last came
// back without a readable decision (no logprobs). A router lives for one
// cycle, so the memo is process-wide: without it a dream chain whose first
// eligible row cannot report logprobs (Ollama) would pay one wasted prefill,
// one error-marked llmlog row and one Info line PER BLOCK, forever. With it
// the decide attempt is skipped while the chain's FIRST row is memoised,
// re-probed after decideIncapableTTL so an upgraded backend is picked up
// again without a restart. Only the no-logprobs signal memoises — a
// no-label-mass answer is a property of one prompt, not of the backend.
var decideIncapable = struct {
	mu   sync.Mutex
	seen map[string]time.Time
}{seen: map[string]time.Time{}}

const decideIncapableTTL = 15 * time.Minute

// noteDecideIncapable records the fallback for the backend that answered.
func noteDecideIncapable(served *backends.Backend, err error) {
	if served == nil || !errors.Is(err, llm.ErrNoLogprobs) {
		return
	}
	decideIncapable.mu.Lock()
	decideIncapable.seen[served.Name] = time.Now()
	decideIncapable.mu.Unlock()
}

// decideCapable reports whether a decide attempt is worth making: the chain
// the call would walk must exist, and its first row must not be memoised as
// incapable within the TTL. A chain error is left to the call itself (its
// error path is the ordinary one).
func decideCapable(r *Router, role string, required backends.Sensitivity) bool {
	chain, err := r.Pool.Chain(role, required, r.Tenant)
	if err != nil || len(chain) == 0 {
		return true
	}
	decideIncapable.mu.Lock()
	defer decideIncapable.mu.Unlock()
	at, ok := decideIncapable.seen[chain[0].Name]
	if !ok {
		return true
	}
	if time.Since(at) > decideIncapableTTL {
		delete(decideIncapable.seen, chain[0].Name)
		return true
	}
	return false
}

// resetDecideIncapable clears the memo (tests).
func resetDecideIncapable() {
	decideIncapable.mu.Lock()
	decideIncapable.seen = map[string]time.Time{}
	decideIncapable.mu.Unlock()
}

// decideStage names the two classifiers for wantDecide.
type decideStage int

const (
	decideStageEval decideStage = iota
	decideStageRecurrence
)

// wantDecide resolves config dream.decide_mode for one classifier from the
// router field. The zero value (a router built without config wiring — every
// test, every caller predating the key) is off, so nothing changes without an
// explicit opt-in; config validation rejects unknown spellings before they
// reach here.
func wantDecide(r *Router, stage decideStage) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.DecideMode)) {
	case DecideModeAll:
		return true
	case DecideModeEval:
		return stage == decideStageEval
	default:
		return false
	}
}

// buildDecideEvalPrompt builds the pairwise prompt: the same header lines,
// guard wraps and content caps as buildEvalPrompt (source MaxContentLen,
// candidate MaxContentLen/2), one nonce binding both wraps and the rule.
func buildDecideEvalPrompt(source, cand BlockInfo) (system, user string) {
	nonce := promptguard.NewNonce()

	var b strings.Builder
	b.WriteString("<source>\n")
	fmt.Fprintf(&b, "ID: %s\nTitle: %s\nCategory: %s\nUpdated: %s\n",
		source.ID, promptguard.GuardLine(source.Title), promptguard.GuardLine(source.Category),
		source.UpdatedAt.Format("2006-01-02"))
	b.WriteString(promptguard.Wrap(nonce, "source",
		promptguard.GuardText(truncate(source.Content, MaxContentLen))))
	b.WriteString("\n</source>\n\n<candidate>\n")
	fmt.Fprintf(&b, "ID: %s\nTitle: %s\nCategory: %s\nUpdated: %s\n",
		cand.ID, promptguard.GuardLine(cand.Title), promptguard.GuardLine(cand.Category),
		cand.UpdatedAt.Format("2006-01-02"))
	b.WriteString(promptguard.Wrap(nonce, "candidate",
		promptguard.GuardText(truncate(cand.Content, MaxContentLen/2))))
	b.WriteString("\n</candidate>")

	return decideEvalSystemPrompt + "\n\n" + promptguard.Rule(nonce), b.String()
}

// decisionToLink maps one decision to at most one link. The relationship is
// the most probable of the four link types; the confidence is the
// probability that ANY link exists, 1 − P(none) — that is the quantity the
// downstream gates (minRawConfidence write gate, graph.min_confidence
// retrieval gate) ask about, and it is a probability read from the model,
// never a parser floor (Floored stays false by construction). No link when
// "none" is the most probable answer.
//
// supersedes is the exception: it is the one relationship with a side effect
// beyond the edge — WriteLinks retires the TARGET block as a snapshot at
// weighted confidence ≥ 0.7 — so its confidence is P(supersedes) itself, not
// "some link exists". A first-token split like supersedes 0.26 / topical 0.25
// / none 0.10 would otherwise retire a block from 26 % belief; with the type's
// own probability the same split fails the 0.7 gate and writes nothing,
// which is what the generating prompt's type-specific confidence did.
func decisionToLink(candID string, d llm.Decision) (Link, bool) {
	if d.Best == decideNoneLabel || d.Best == "" {
		return Link{}, false
	}
	bestType, bestP := "", -1.0
	for _, label := range decideLinkLabels {
		rel, ok := decideLinkTypes[label]
		if !ok {
			continue
		}
		if p := d.Probs[label]; p > bestP {
			bestType, bestP = rel, p
		}
	}
	conf := 1 - d.Probs[decideNoneLabel]
	if bestType == "supersedes" {
		conf = bestP
	}
	return Link{
		TargetID:     candID,
		Relationship: bestType,
		Confidence:   conf,
	}, true
}

// stampDecision writes the decision read-out onto the llmlog row so
// calibration can be measured afterwards (metadata.decide_probs keyed by the
// label NAMES the classifier uses — relationship types resp. verdicts — plus
// mass/confidence/best). Labels absent from names keep their letter.
func stampDecision(entry *llmlog.Entry, d llm.Decision, names map[string]string) {
	if entry.Metadata == nil {
		entry.Metadata = map[string]any{}
	}
	probs := make(map[string]float64, len(d.Probs))
	for label, p := range d.Probs {
		key := names[label]
		if key == "" {
			key = label
		}
		probs[key] = p
	}
	entry.Metadata["decide_probs"] = probs
	entry.Metadata["decide_mass"] = d.Mass
	entry.Metadata["decide_confidence"] = d.Confidence
	entry.Metadata["decide_best"] = d.Best
}

// evaluateRelationshipsDecide is the decide-mode twin of evalAttempt: one
// wire call and one llmlog row per candidate, the pair's sensitivity fold
// (source + this candidate, not all of them), then the shared post-parse
// constraints over the collected links. A wire error on any pair fails the
// evaluation like a wire error on the single generating call does (transient
// cooldown at the call site); a fallback signal returns errDecideFallback
// after the rows of the pairs already judged were recorded.
func evaluateRelationshipsDecide(ctx context.Context, pool *pgxpool.Pool, r *Router, source BlockInfo, candidates []BlockInfo, capped int) ([]Link, error) {
	links := make([]Link, 0, len(candidates))
	for i := range candidates {
		link, ok, err := decideOnePair(ctx, pool, r, source, candidates[i], capped)
		if err != nil {
			return nil, err
		}
		if ok {
			links = append(links, link)
		}
	}
	return finishDecideLinks(source, candidates, links), nil
}

// decideOnePair is ONE decide wire call with its own llmlog row.
func decideOnePair(ctx context.Context, pool *pgxpool.Pool, r *Router, source, cand BlockInfo, capped int) (Link, bool, error) {
	system, user := buildDecideEvalPrompt(source, cand)
	required := backends.MaxSensitivity(source.Sensitivity, cand.Sensitivity)

	entry := newDreamEntry("dream-eval", system, user, []string{source.ID, cand.ID}, promptDecideEval)
	noteCandidatesCapped(entry, capped)
	if entry.Metadata == nil {
		entry.Metadata = map[string]any{}
	}
	entry.Metadata["decide"] = true
	defer func() { llmlog.Record(pool, entry.Slimmed(r.Devmode)) }()

	start := time.Now()
	// chatPlain: a JSON-mode grammar would forbid the bare letter.
	resp, served, attempts, err := r.chatPlain(ctx, backends.RoleDream, required,
		system, user, llm.DecideOptions(), DreamTimeout)
	entry.Duration = time.Since(start)
	entry.Err = err
	r.applyChainTelemetry(entry, backends.RoleDream, required, served, resp, attempts, err)
	if resp != nil {
		entry.ResponseContent = resp.Message.Content
		entry.CompletionTokens = resp.EvalCount
		entry.PromptTokens = resp.PromptTokens
		if resp.FinishReason != "" {
			entry.Metadata["finish_reason"] = resp.FinishReason
		}
	}
	if err != nil {
		return Link{}, false, fmt.Errorf("dream: evaluate (decide): %w", err)
	}

	d, err := llm.Decide(resp, decideLinkLabels)
	if err != nil {
		entry.Err = fmt.Errorf("decide: %w", err)
		if llm.IsDecideFallback(err) {
			entry.Metadata["decide_fallback"] = err.Error()
			noteDecideIncapable(served, err)
			return Link{}, false, fmt.Errorf("%w: %w (backend %s)", errDecideFallback, err, backendName(served))
		}
		return Link{}, false, fmt.Errorf("dream: evaluate (decide): %w", err)
	}
	stampDecision(entry, d, decideLinkNames)
	entry.Metadata["parse_format"] = "decide"
	link, ok := decisionToLink(cand.ID, d)
	if ok {
		entry.Metadata["links_parsed"] = 1
	} else {
		entry.Metadata["links_parsed"] = 0
	}
	return link, ok, nil
}

// finishDecideLinks runs the post-parse constraints the generating path runs
// after parseLinks — minus applyLinkFloor, which lifts parser-floored links
// only and a decision is never floored. Shared with the goldbench decide axis
// so the bench scores exactly what production would write.
//
// Telemetry: the generating path stamps supersedes_direction_downgraded,
// links_dropped_invalid and links_capped on its ONE llmlog row. Decide mode
// has one row per pair and these counts are per block, so they go to a
// structured log line instead (one per block with at least one decided link)
// — grep "dream: decide links finished" for the gate effects.
func finishDecideLinks(source BlockInfo, candidates []BlockInfo, links []Link) []Link {
	candidateIDs := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		candidateIDs[c.ID] = true
	}
	decided := len(links)
	links, downgraded := enforceSupersedesDirection(links, source.CreatedAt, candidates)
	valid := filterValidCandidates(links, candidateIDs)
	capped, cappedN := applyHardCap(valid, MaxLinksPerCycle)
	if decided > 0 {
		slog.Info("dream: decide links finished",
			"block_id", source.ID, "candidates", len(candidates), "decided", decided,
			"supersedes_direction_downgraded", downgraded,
			"links_dropped_invalid", decided-len(valid), "links_capped", cappedN, "written", len(capped))
	}
	return capped
}
