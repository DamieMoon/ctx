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
	// confidence = 1 − P(none) = 0.90, not P(recurrent)
	if v.Verdict != "recurrent" || math.Abs(v.Confidence-0.90) > 1e-9 {
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
	if err != nil || v.Verdict != "none" || math.Abs(v.Confidence-0.3) > 1e-9 {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestDecideRecurrence_NoLogprobs_IsFallbackSignal(t *testing.T) {
	resetDecideIncapable()
	t.Cleanup(resetDecideIncapable)
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
	// supersedes carries P(supersedes), not 1 − P(none).
	v := decisionToRecurrenceVerdict(llm.Decision{Best: "B", Probs: map[string]float64{"A": 0.2, "B": 0.75, "C": 0.05}})
	if v.Verdict != "supersedes" || math.Abs(v.Confidence-0.75) > 1e-9 || v.Pattern != "" {
		t.Fatalf("%+v", v)
	}
	v = decisionToRecurrenceVerdict(llm.Decision{Best: "B", Probs: map[string]float64{"A": 0.33, "B": 0.35, "C": 0.32}})
	if v.Verdict != "supersedes" || math.Abs(v.Confidence-0.35) > 1e-9 || v.Confidence >= recurrenceWriteFloor(true, "supersedes") {
		t.Fatalf("a 35 %% supersedes split must not pass the 0.7 gate: %+v", v)
	}
	// Best is a link label but supersedes edges out recurrent only when it is
	// the more probable of the two.
	v = decisionToRecurrenceVerdict(llm.Decision{Best: "A", Probs: map[string]float64{"A": 0.45, "B": 0.15, "C": 0.40}})
	if v.Verdict != "recurrent" || math.Abs(v.Confidence-0.60) > 1e-9 {
		t.Fatalf("%+v", v)
	}
}

func TestRecurrenceWriteFloor(t *testing.T) {
	if recurrenceWriteFloor(true, "recurrent") != DecideRecurrenceFloor {
		t.Fatal("decided recurrent uses the decide floor")
	}
	if recurrenceWriteFloor(true, "supersedes") != 0.7 {
		t.Fatal("decided supersedes keeps minRawConfidence — it retires a block")
	}
	if recurrenceWriteFloor(false, "recurrent") != 0.8 || recurrenceWriteFloor(false, "supersedes") != 0.7 {
		t.Fatal("generated verdicts keep minRawConfidence")
	}
	// The floor sits at the argmax boundary: P(link)=0.5 passes, 0.49 does not.
	if !(0.5 >= DecideRecurrenceFloor) || 0.49 >= DecideRecurrenceFloor {
		t.Fatal("floor is the argmax boundary 0.5")
	}
}
