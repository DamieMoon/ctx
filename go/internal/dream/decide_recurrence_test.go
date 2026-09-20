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

func recCand(id string) recurrenceCandidate {
	return recurrenceCandidate{TargetID: id, TargetTitle: "Report KW 12", TargetText: "weekly report", TitleSim: 0.82}
}

func TestDecideRecurrence_VerdictAndConfidence(t *testing.T) {
	calls := decideSeam(t, &llm.ChatResponse{
		Message: llm.Message{Role: "assistant", Content: "A"}, EvalCount: 1, PromptTokens: 250, FinishReason: "length",
		TopLogprobs: logprobsFor(map[string]float64{"A": 0.85, "C": 0.10, "B": 0.05}),
	})
	v, err := confirmRecurrenceDecide(context.Background(), nil, decideRouter(DecideModeAll), srcBlock(uuidA), recCand(uuidB))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Verdict != "recurrent" || math.Abs(v.Confidence-0.85) > 1e-9 {
		t.Fatalf("verdict = %+v", v)
	}
	c0 := (*calls)[0]
	if c0.opts.NumPredict != 1 || c0.opts.TopLogprobs != 20 {
		t.Fatalf("decide options on the wire = %+v", c0.opts)
	}
	if !strings.Contains(c0.system, "A = recurrent") || !strings.Contains(c0.user, "title_sim=\"0.82\"") {
		t.Fatalf("prompt shape: system=%q user=%q", c0.system, c0.user)
	}
}

func TestDecideRecurrence_NoneVerdict(t *testing.T) {
	decideSeam(t, &llm.ChatResponse{
		Message: llm.Message{Role: "assistant", Content: "C"}, EvalCount: 1,
		TopLogprobs: logprobsFor(map[string]float64{"C": 0.7, "A": 0.3}),
	})
	v, err := confirmRecurrenceDecide(context.Background(), nil, decideRouter(DecideModeAll), srcBlock(uuidA), recCand(uuidB))
	if err != nil || v.Verdict != "none" || math.Abs(v.Confidence-0.7) > 1e-9 {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestDecideRecurrence_NoLogprobs_IsFallbackSignal(t *testing.T) {
	decideSeam(t, &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: "A"}, EvalCount: 1})
	_, err := confirmRecurrenceDecide(context.Background(), nil, decideRouter(DecideModeAll), srcBlock(uuidA), recCand(uuidB))
	if !errors.Is(err, errDecideFallback) || !errors.Is(err, llm.ErrNoLogprobs) {
		t.Fatalf("want errDecideFallback wrapping ErrNoLogprobs, got %v", err)
	}
}

func TestDecideRecurrence_WireError_NotFallback(t *testing.T) {
	mockChatJSON(t, func(context.Context, string, string, string, *bool, string, string, llm.Options, time.Duration) (*llm.ChatResponse, error) {
		return nil, errors.New("boom")
	})
	_, err := confirmRecurrenceDecide(context.Background(), nil, decideRouter(DecideModeAll), srcBlock(uuidA), recCand(uuidB))
	if err == nil || errors.Is(err, errDecideFallback) {
		t.Fatalf("wire error must propagate as-is, got %v", err)
	}
}

func TestDecisionToRecurrenceVerdict(t *testing.T) {
	v := decisionToRecurrenceVerdict(llm.Decision{Best: "B", Probs: map[string]float64{"A": 0.2, "B": 0.75, "C": 0.05}})
	if v.Verdict != "supersedes" || v.Confidence != 0.75 || v.Pattern != "" {
		t.Fatalf("%+v", v)
	}
}
