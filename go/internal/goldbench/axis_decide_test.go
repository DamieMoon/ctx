package goldbench

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/GottZ/ctx/internal/llm"
)

// decideTops is a first-token read-out with P(topical)=p, P(none)=1−p.
func decideTops(p float64) []llm.TokenLogprob {
	return []llm.TokenLogprob{{Token: "D", Logprob: math.Log(p)}, {Token: "E", Logprob: math.Log(1 - p)}}
}

// TestScoreLinksDecide_PrecisionSeesTheTieRule pins why the axis reports
// link_precision next to link_score: the gold link (0.97, the anchor) is hit
// either way, so link_score is 1.0 with and without the tie rule, while the
// rule drops the non-tie 0.80 link and only precision shows it.
func TestScoreLinksDecide_PrecisionSeesTheTieRule(t *testing.T) {
	const (
		src  = "019d0000-0000-7000-8000-000000000001"
		gold = "019d0000-0000-7000-8000-000000000002"
		far  = "019d0000-0000-7000-8000-000000000003"
	)
	block := func(id string) blockJSON {
		return blockJSON{ID: id, Title: "t " + id, Category: "learnings", Updated: "2026-10-01", Content: "c"}
	}
	in, _ := json.Marshal(linksInput{Source: block(src), Candidates: []blockJSON{block(gold), block(far)}})
	g, _ := json.Marshal(map[string]any{"links": []map[string]string{{"target_id": gold, "type": "topical"}}})
	run := caseRun{
		c:    &Case{ID: "gb-links-test", Input: in, Gold: g},
		tops: [][]llm.TokenLogprob{decideTops(0.97), decideTops(0.80)},
	}

	res, _ := scoreLinksDecide([]caseRun{run})
	sec := res.Secondary
	if res.PrimaryScore != 1 || sec["link_score_no_tie"] != 1 {
		t.Fatalf("link_score %v / no_tie %v, want 1 / 1", res.PrimaryScore, sec["link_score_no_tie"])
	}
	if sec["link_precision"] != 1 || sec["link_precision_no_tie"] != 0.5 {
		t.Fatalf("precision %v / no_tie %v, want 1 / 0.5", sec["link_precision"], sec["link_precision_no_tie"])
	}
	if sec["link_recall"] != 1 || sec["link_recall_no_tie"] != 1 {
		t.Fatalf("recall %v / no_tie %v, want 1 / 1", sec["link_recall"], sec["link_recall_no_tie"])
	}
}
