package goldbench

import (
	"encoding/json"
	"fmt"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
)

// Decide-Achsen (2026-09-20): die beiden Dream-Klassifikatoren im
// Prefill-only-Modus (dream.decide_mode), gemessen gegen dieselben Goldsets
// und mit denselben Scorern wie ihre generierenden Basisachsen — links-decide
// teilt scoreLinksCase, recurrence-decide die 3-Klassen-Accuracy. Prompts,
// Label-Vokabular und Link-Abbildung kommen über Bench-Shims aus dem
// dream-Paket, damit die Achse exakt das bewertet, was die Produktion
// schreiben würde (inklusive Typ-Gate und Hard-Cap).
//
// Prospektiv, weil der Modus per Default aus ist; sobald er promotet wird,
// tauschen Basis- und Decide-Achse ihre Rolle, nicht ihren Scorer.
//
// Ein Request ohne Top-Logprobs (Server ignoriert das Feld) ist in Produktion
// das Fallback-Signal auf das generierende Prompt; hier zählt er als
// „unparsebar" (Score 0 für den Fall) und wird unter fallback_rate berichtet
// — die Achse misst den Decide-Pfad, nicht den Fallback.

// decideSampling ist llm.DecideOptions auf der Bench-Wire: greedy, ein Token,
// Top-20-Alternativen.
func decideSampling() SamplingOpts {
	o := llm.DecideOptions()
	return SamplingOpts{Temperature: o.Temperature, MaxTokens: o.NumPredict, TopLogprobs: o.TopLogprobs}
}

// decideAt liest die Entscheidung des Request-Slots i; nil, wenn der Slot
// keine Top-Logprobs trägt oder keine Label-Masse hat (Fallback-Signal).
func decideAt(r caseRun, i int, labels []string) *llm.Decision {
	if i >= len(r.tops) || len(r.tops[i]) == 0 {
		return nil
	}
	d, err := llm.DecideChoice(r.tops[i], labels)
	if err != nil {
		return nil
	}
	return &d
}

func axisLinksDecide() axisDef {
	return axisDef{
		name:        "links-decide",
		prospective: true,
		data:        "links",
		build: func(c *Case) ([]ChatRequest, error) {
			var in linksInput
			if err := decodeInto(c.Input, &in, "links-decide input", c.ID); err != nil {
				return nil, err
			}
			source, err := in.Source.toBlockInfo()
			if err != nil {
				return nil, fmt.Errorf("goldbench: links-decide %s: source: %w", c.ID, err)
			}
			reqs := make([]ChatRequest, 0, len(in.Candidates))
			for i, cand := range in.Candidates {
				bi, err := cand.toBlockInfo()
				if err != nil {
					return nil, fmt.Errorf("goldbench: links-decide %s: candidate %d: %w", c.ID, i, err)
				}
				system, user := dream.BenchBuildDecideEvalPrompt(source, bi)
				reqs = append(reqs, ChatRequest{System: system, User: user, Opts: decideSampling()})
			}
			return reqs, nil
		},
		score: scoreLinksDecide,
	}
}

// scoreLinksDecide: Primärmetrik link_score wie scoreLinks, berechnet auf den
// Links, die die Produktion schreiben würde (BenchDecideLinks: argmax-Typ,
// Confidence 1−P(none), Typ-Gate, Hard-Cap). Sekundär: link_score_argmax
// (Roh-Argmax ohne Gate, die Bench-Sicht des Vorabreports), mean_mass, mean
// P(link) und fallback_rate (Slots ohne lesbare Entscheidung).
func scoreLinksDecide(runs []caseRun) (AxisResult, []CaseScore) {
	labels := dream.BenchDecideLinkLabels()
	var scores, argmaxScores, masses, plinks []float64
	parsed, slots, fallbacks := 0, 0, 0
	confusion := map[string]map[string]int{}
	bump := func(gold, pred string) {
		if confusion[gold] == nil {
			confusion[gold] = map[string]int{}
		}
		confusion[gold][pred]++
	}
	noBump := func(string, string) {}
	perCase := make([]CaseScore, 0, len(runs))

	for _, r := range runs {
		var in linksInput
		_ = json.Unmarshal(r.c.Input, &in)
		var gold linksGold
		_ = json.Unmarshal(r.c.Gold, &gold)
		cs := CaseScore{ID: r.c.ID}

		source, err := in.Source.toBlockInfo()
		if err != nil {
			scores = append(scores, 0)
			perCase = append(perCase, cs)
			continue
		}
		candidates := make([]dream.BlockInfo, 0, len(in.Candidates))
		for _, cand := range in.Candidates {
			bi, _ := cand.toBlockInfo()
			candidates = append(candidates, bi)
		}
		decisions := make([]*llm.Decision, len(candidates))
		complete := true
		var rawLinks []dream.Link
		for i := range candidates {
			slots++
			d := decideAt(r, i, labels)
			if d == nil {
				fallbacks++
				complete = false
				continue
			}
			decisions[i] = d
			masses = append(masses, d.Mass)
			plinks = append(plinks, 1-d.Probs["E"])
			if rel := dream.BenchDecideLinkArgmax(*d); rel != "none" {
				rawLinks = append(rawLinks, dream.Link{TargetID: candidates[i].ID, Relationship: rel, Confidence: 1})
			}
		}
		if !complete {
			// Ein Fall ohne vollständige Entscheidung ist kein Decide-Ergebnis.
			scores = append(scores, 0)
			perCase = append(perCase, cs)
			continue
		}
		parsed++
		cs.Parsed = true
		links := dream.BenchDecideLinks(source, candidates, decisions)
		cs.Score = scoreLinksCase(gold, links, bump)
		scores = append(scores, cs.Score)
		argmaxScores = append(argmaxScores, scoreLinksCase(gold, rawLinks, noBump))
		perCase = append(perCase, cs)
	}
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(parsed, len(runs)),
		PrimaryMetric: "link_score",
		PrimaryScore:  meanOrZero(scores),
		Secondary: map[string]float64{
			"link_score_argmax": meanOrZero(argmaxScores),
			"mean_mass":         meanOrZero(masses),
			"mean_p_link":       meanOrZero(plinks),
			"fallback_rate":     ratioOrZero(fallbacks, slots),
		},
		Confusion: confusion,
	}, perCase
}

func axisRecurrenceDecide() axisDef {
	return axisDef{
		name:        "recurrence-decide",
		prospective: true,
		data:        "recurrence",
		build: func(c *Case) ([]ChatRequest, error) {
			var in recurrenceInput
			if err := decodeInto(c.Input, &in, "recurrence-decide input", c.ID); err != nil {
				return nil, err
			}
			source, err := in.BlockA.toBlockInfo()
			if err != nil {
				return nil, fmt.Errorf("goldbench: recurrence-decide %s: block_a: %w", c.ID, err)
			}
			sim := trgmSimilarity(in.BlockA.Title, in.BlockB.Title)
			system, user := dream.BenchBuildDecideRecurrencePrompt(source,
				in.BlockB.ID, in.BlockB.Title, in.BlockB.Content, sim)
			return []ChatRequest{{System: system, User: user, Opts: decideSampling()}}, nil
		},
		score: scoreRecurrenceDecide,
	}
}

// scoreRecurrenceDecide: accuracy_3class auf dem Roh-Verdict wie
// scoreRecurrence, plus none_fp_rate; sekundär accuracy_gated (der Verdict,
// den der Loop nach dem Typ-Gate schreiben würde — gegateter recurrent/
// supersedes zählt als none) und mean_mass.
func scoreRecurrenceDecide(runs []caseRun) (AxisResult, []CaseScore) {
	labels := dream.BenchDecideRecurrenceLabels()
	parsed, correct, correctGated := 0, 0, 0
	noneTotal, noneFP := 0, 0
	var masses []float64
	confusion := map[string]map[string]int{}
	perCase := make([]CaseScore, 0, len(runs))
	for _, r := range runs {
		var gold recurrenceGold
		_ = json.Unmarshal(r.c.Gold, &gold)
		if gold.Verdict == "none" {
			noneTotal++
		}
		cs := CaseScore{ID: r.c.ID}
		d := decideAt(r, 0, labels)
		if d == nil {
			perCase = append(perCase, cs)
			continue
		}
		parsed++
		cs.Parsed = true
		masses = append(masses, d.Mass)
		verdict, _, gated := dream.BenchDecideRecurrenceVerdict(*d)
		if confusion[gold.Verdict] == nil {
			confusion[gold.Verdict] = map[string]int{}
		}
		confusion[gold.Verdict][verdict]++
		if verdict == gold.Verdict {
			correct++
			cs.Score = 1
		}
		written := "none"
		if gated {
			written = verdict
		}
		if written == gold.Verdict {
			correctGated++
		}
		if gold.Verdict == "none" && (verdict == "recurrent" || verdict == "supersedes") {
			noneFP++
		}
		perCase = append(perCase, cs)
	}
	return AxisResult{
		N:             len(runs),
		ParseRate:     ratioOrZero(parsed, len(runs)),
		PrimaryMetric: "accuracy_3class",
		PrimaryScore:  ratioOrZero(correct, len(runs)),
		Secondary: map[string]float64{
			"none_fp_rate":   ratioOrZero(noneFP, noneTotal),
			"accuracy_gated": ratioOrZero(correctGated, len(runs)),
			"mean_mass":      meanOrZero(masses),
		},
		Confusion: confusion,
	}, perCase
}
