package dream

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llm"
)

// decideRouter is newTestRouter with decide mode armed.
func decideRouter(mode string) *Router {
	r := newTestRouter()
	r.DecideMode = mode
	return r
}

// logprobsFor builds a first-token top-logprobs list from label→probability.
func logprobsFor(probs map[string]float64) []llm.TokenLogprob {
	out := make([]llm.TokenLogprob, 0, len(probs))
	for tok, p := range probs {
		out = append(out, llm.TokenLogprob{Token: tok, Logprob: math.Log(p)})
	}
	return out
}

// decideSeam installs a chatJSON seam answering call i with answers[i] (the
// last answer repeats). Every call is recorded: system prompt, user prompt and
// the resolved Options, so a test can pin the wire shape of a decide call.
type decideCall struct {
	system, user string
	opts         llm.Options
}

func decideSeam(t *testing.T, answers ...*llm.ChatResponse) *[]decideCall {
	t.Helper()
	calls := make([]decideCall, 0, len(answers))
	mockChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, sys, usr string, opts llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		calls = append(calls, decideCall{system: sys, user: usr, opts: opts})
		a := answers[len(answers)-1]
		if len(calls) <= len(answers) {
			a = answers[len(calls)-1]
		}
		return a, nil
	})
	return &calls
}

func decideResp(probs map[string]float64) *llm.ChatResponse {
	return &llm.ChatResponse{
		Message:      llm.Message{Role: "assistant", Content: "D"},
		EvalCount:    1,
		PromptTokens: 300,
		FinishReason: "length",
		TopLogprobs:  logprobsFor(probs),
	}
}

func TestDecideEval_HappyPath_OneRowPerCandidate(t *testing.T) {
	calls := decideSeam(t,
		decideResp(map[string]float64{"D": 0.80, "E": 0.15, "C": 0.05}), // topical, P(link)=0.85
		decideResp(map[string]float64{"E": 0.90, "D": 0.10}),            // none
		decideResp(map[string]float64{"C": 0.50, "D": 0.30, "E": 0.20}), // factual, P(link)=0.80
	)
	links, err := EvaluateRelationships(context.Background(), nil, decideRouter(DecideModeEval), DreamOptions(),
		srcBlock(uuidA), []BlockInfo{candBlock(uuidB), candBlock(uuidC), candBlock(uuidD)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 3 {
		t.Fatalf("calls = %d, want one per candidate", len(*calls))
	}
	if len(links) != 2 {
		t.Fatalf("links = %+v, want 2", links)
	}
	if links[0].TargetID != uuidB || links[0].Relationship != "topical" || math.Abs(links[0].Confidence-0.85) > 1e-9 || links[0].Floored {
		t.Fatalf("link 0 = %+v", links[0])
	}
	if links[1].TargetID != uuidD || links[1].Relationship != "factual" || math.Abs(links[1].Confidence-0.80) > 1e-9 {
		t.Fatalf("link 1 = %+v", links[1])
	}
	// Wire shape of a decide call: one token, top-20 alternatives, greedy,
	// pairwise prompt (the candidate id appears, the other candidates do not).
	c0 := (*calls)[0]
	if c0.opts.NumPredict != 1 || c0.opts.TopLogprobs != 20 || c0.opts.Temperature != 0 {
		t.Fatalf("decide options on the wire = %+v", c0.opts)
	}
	if !strings.Contains(c0.user, uuidB) || strings.Contains(c0.user, uuidC) {
		t.Fatalf("pairwise prompt must carry exactly its candidate: %s", c0.user)
	}
	if !strings.Contains(c0.system, "A = supersedes") {
		t.Fatalf("decide system prompt missing letter contract")
	}
}

func TestDecideEval_ConfidenceIsLinkProbability_GateApplies(t *testing.T) {
	// D wins the argmax, but P(none)=0.40 → P(link)=0.60 < minRawConfidence
	// 0.7: the link is dropped by the shared write gate, as production would.
	decideSeam(t, decideResp(map[string]float64{"D": 0.60, "E": 0.40}))
	links, err := EvaluateRelationships(context.Background(), nil, decideRouter(DecideModeAll), DreamOptions(),
		srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("below-gate link must be dropped, got %+v", links)
	}
}

func TestDecideEval_SupersedesDirectionDowngraded(t *testing.T) {
	// Source created BEFORE the candidate was updated → supersedes is
	// direction-inverted and downgrades to topical (Welle 46 constraint).
	decideSeam(t, decideResp(map[string]float64{"A": 0.90, "E": 0.10}))
	links, err := EvaluateRelationships(context.Background(), nil, decideRouter(DecideModeEval), DreamOptions(),
		srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 1 || links[0].Relationship != "topical" {
		t.Fatalf("want direction downgrade to topical, got %+v", links)
	}
}

func TestDecideEval_HardCapFive(t *testing.T) {
	decideSeam(t, decideResp(map[string]float64{"D": 0.95, "E": 0.05}))
	cands := []BlockInfo{candBlock(uuidB), candBlock(uuidC), candBlock(uuidD), candBlock(uuidE), candBlock(uuidF), candBlock(uuidG)}
	links, err := EvaluateRelationships(context.Background(), nil, decideRouter(DecideModeEval), DreamOptions(), srcBlock(uuidA), cands)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != MaxLinksPerCycle {
		t.Fatalf("links = %d, want hard cap %d", len(links), MaxLinksPerCycle)
	}
}

func TestDecideEval_NoLogprobs_FallsBackToGeneration(t *testing.T) {
	// First call: a backend that ignored top_logprobs. The block is then
	// re-evaluated through the generating path, whose answer is a JSON array.
	calls := decideSeam(t,
		&llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: "D"}, EvalCount: 1, PromptTokens: 300},
		&llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: `[{"target_id":"` + uuidB + `","type":"topical","confidence":0.9}]`}, EvalCount: 20, PromptTokens: 500},
	)
	links, err := EvaluateRelationships(context.Background(), nil, decideRouter(DecideModeEval), DreamOptions(),
		srcBlock(uuidA), []BlockInfo{candBlock(uuidB), candBlock(uuidC)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want 1 decide attempt + 1 generating fallback", len(*calls))
	}
	if (*calls)[1].opts.TopLogprobs != 0 || (*calls)[1].opts.NumPredict != DefaultNumPredict {
		t.Fatalf("fallback must be the generating call: %+v", (*calls)[1].opts)
	}
	if len(links) != 1 || links[0].TargetID != uuidB || links[0].Relationship != "topical" {
		t.Fatalf("fallback links = %+v", links)
	}
}

func TestDecideEval_ThinkingToken_FallsBack(t *testing.T) {
	// Logprobs present but the first token is a thinking tag: no label mass →
	// fallback, not a fabricated decision.
	calls := decideSeam(t,
		decideResp(map[string]float64{"<think>": 0.99}),
		&llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: "[]"}, EvalCount: 2, PromptTokens: 500},
	)
	links, err := EvaluateRelationships(context.Background(), nil, decideRouter(DecideModeEval), DreamOptions(),
		srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil || len(links) != 0 || len(*calls) != 2 {
		t.Fatalf("links=%+v err=%v calls=%d", links, err, len(*calls))
	}
}

func TestDecideEval_WireError_Propagates(t *testing.T) {
	mockChatJSON(t, func(context.Context, string, string, string, *bool, string, string, llm.Options, time.Duration) (*llm.ChatResponse, error) {
		return nil, errors.New("boom")
	})
	_, err := EvaluateRelationships(context.Background(), nil, decideRouter(DecideModeEval), DreamOptions(),
		srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err == nil || !strings.Contains(err.Error(), "dream: evaluate (decide)") {
		t.Fatalf("want wrapped wire error, got %v", err)
	}
}

func TestDecideEval_ModeOff_UsesGeneration(t *testing.T) {
	calls := decideSeam(t, &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: "[]"}, EvalCount: 2, PromptTokens: 500})
	for _, mode := range []string{"", DecideModeOff} {
		*calls = (*calls)[:0]
		_, err := EvaluateRelationships(context.Background(), nil, decideRouter(mode), DreamOptions(),
			srcBlock(uuidA), []BlockInfo{candBlock(uuidB), candBlock(uuidC)})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if len(*calls) != 1 || (*calls)[0].opts.TopLogprobs != 0 {
			t.Fatalf("mode %q must make ONE generating call, got %d (%+v)", mode, len(*calls), *calls)
		}
	}
}

func TestDecisionToLink_ArgmaxTypeAndLinkProbability(t *testing.T) {
	d := llm.Decision{Best: "D", Probs: map[string]float64{"A": 0.05, "B": 0.10, "C": 0.30, "D": 0.35, "E": 0.20}}
	l, ok := decisionToLink(uuidB, d)
	if !ok || l.Relationship != "topical" || math.Abs(l.Confidence-0.80) > 1e-9 || l.Floored {
		t.Fatalf("%+v %v", l, ok)
	}
	if _, ok := decisionToLink(uuidB, llm.Decision{Best: "E", Probs: map[string]float64{"E": 0.9, "D": 0.1}}); ok {
		t.Fatal("none must yield no link")
	}
}

func TestWantDecide(t *testing.T) {
	cases := []struct {
		mode      string
		eval, rec bool
	}{
		{"", false, false}, {"off", false, false}, {"eval", true, false}, {"all", true, true}, {" ALL ", true, true},
	}
	for _, c := range cases {
		r := &Router{DecideMode: c.mode}
		if got := wantDecide(r, decideStageEval); got != c.eval {
			t.Fatalf("mode %q eval = %v", c.mode, got)
		}
		if got := wantDecide(r, decideStageRecurrence); got != c.rec {
			t.Fatalf("mode %q recurrence = %v", c.mode, got)
		}
	}
	if wantDecide(nil, decideStageEval) {
		t.Fatal("nil router must be off")
	}
}
