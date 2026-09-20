package dream

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/prompts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// decideRecurrenceSystemPrompt is the recurrence confirm as a letter
// contract: the Welle-38b type definitions verbatim, the JSON output line
// replaced. Phase 2 was already one call per pair, so the request count does
// not change — only its answer shrinks to one token.
const decideRecurrenceSystemPrompt = `Classify whether two knowledge blocks form a recurring pattern.

Block A and Block B share a temporal-dimension value AND a similar title shape.
Decide which relationship type applies — or skip it.

Types:
- recurrent: B is another instance of the same pattern as A. Both are valid in parallel.
  Patterns:
    parallel — different concrete instances of one class (e.g. mautrix-signal vs mautrix-discord bridges)
    sequence — sequential phases or periodisations of one body of work (e.g. Phase 1 vs Phase 4 of a project)
    weekly / monthly / sessional — periodic reports/handovers of the same activity
- supersedes: B explicitly replaces A. Use only when B is the newer authoritative version of the same fact AND A is wrong/obsolete (NOT just older). RARE.
- none: A and B share temporal+title shape but treat different topics, OR they are merely topically related without a pattern.

When in doubt: none.
Answer with exactly one letter and nothing else: A = recurrent, B = supersedes, C = none.`

// promptDecideRecurrence is the identity of the decide-mode recurrence confirm.
var promptDecideRecurrence = prompts.Register(
	"dream.decideRecurrenceSystemPrompt", "2026-09-20", "github.com/GottZ/ctx/internal/dream")

var (
	decideRecurrenceLabels = []string{"A", "B", "C"}
	decideRecurrenceNames  = map[string]string{
		"A": "recurrent",
		"B": "supersedes",
		"C": "none",
	}
)

// buildDecideRecurrencePrompt is buildRecurrencePrompt under the letter
// contract: identical header lines, wraps, caps and nonce discipline.
func buildDecideRecurrencePrompt(source BlockInfo, c recurrenceCandidate) (system, user string) {
	nonce := promptguard.NewNonce()

	var b strings.Builder
	fmt.Fprintf(&b, "block_a: id=%s title=\"%s\" updated=\"%s\"\n",
		source.ID, promptguard.GuardLine(source.Title), source.UpdatedAt.Format("2006-01-02"))
	b.WriteString(promptguard.Wrap(nonce, "block_a",
		promptguard.GuardText(truncate(source.Content, MaxContentLen))))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "block_b: id=%s title=\"%s\" title_sim=\"%.2f\"\n",
		c.TargetID, promptguard.GuardLine(c.TargetTitle), c.TitleSim)
	b.WriteString(promptguard.Wrap(nonce, "block_b",
		promptguard.GuardText(truncate(c.TargetText, MaxContentLen))))

	return decideRecurrenceSystemPrompt + "\n\n" + promptguard.Rule(nonce), b.String()
}

// decisionToRecurrenceVerdict maps the decision to the verdict shape the
// DetectRecurrence loop already consumes: the most probable label is the
// verdict, its probability the confidence (the loop's per-type gate reads it
// — 0.8 for recurrent, 0.7 for supersedes). Pattern is not derivable from a
// three-way answer and is never persisted anyway.
func decisionToRecurrenceVerdict(d llm.Decision) recurrenceVerdict {
	return recurrenceVerdict{
		Verdict:    decideRecurrenceNames[d.Best],
		Confidence: d.Probs[d.Best],
	}
}

// confirmRecurrenceDecide is confirmRecurrence's decide-mode twin: one plain
// wire call with DecideOptions, one dream-recurrence llmlog row, the answer
// read from the top-logprobs. A fallback signal (no logprobs, no label mass)
// returns errDecideFallback so the loop can re-ask the generating prompt for
// the same pair.
func confirmRecurrenceDecide(ctx context.Context, pool *pgxpool.Pool, r *Router, source BlockInfo, c recurrenceCandidate) (recurrenceVerdict, error) {
	system, user := buildDecideRecurrencePrompt(source, c)
	required := backends.MaxSensitivity(source.Sensitivity, c.TargetSens)

	entry := newDreamEntry("dream-recurrence", system, user, []string{source.ID, c.TargetID}, promptDecideRecurrence)
	if entry.Metadata == nil {
		entry.Metadata = map[string]any{}
	}
	entry.Metadata["decide"] = true
	defer func() { llmlog.Record(pool, entry.Slimmed(r.Devmode)) }()

	start := time.Now()
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
		return recurrenceVerdict{}, err
	}

	d, err := llm.Decide(resp, decideRecurrenceLabels)
	if err != nil {
		entry.Err = fmt.Errorf("decide: %w", err)
		if llm.IsDecideFallback(err) {
			entry.Metadata["decide_fallback"] = err.Error()
			return recurrenceVerdict{}, fmt.Errorf("%w: %w (backend %s)", errDecideFallback, err, backendName(served))
		}
		return recurrenceVerdict{}, err
	}
	stampDecision(entry, d, decideRecurrenceNames)
	entry.Metadata["parse_format"] = "decide"
	return decisionToRecurrenceVerdict(d), nil
}
