package llm

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// The decide primitive: a classification read off the FIRST generated token's
// top-logprobs instead of decoded text (System-One style, TypeSafe "Jev";
// design evidence .project/bench-jev-2026-09-20/REPORT.md). The caller phrases
// its question so the answer is exactly one short label token ("A" … "E",
// "Yes"/"No"); the provider returns the alternatives it considered for that
// position; DecideChoice turns them into a probability distribution over the
// caller's labels.
//
// Why: on the serving hardware one output token costs ~100× an input token
// (decode 18–30 tok/s vs. prefill 2.5k tok/s), and a decision needs no
// output token at all. It also removes the parse layer: there is no JSON to
// truncate, fence or drift. What it does NOT change is the prompt budget —
// every judged item still pays its prefill.
//
// Calibration caveat: an instruct model's first-token distribution is a
// usable RANKING signal and a threshold you can measure against gold, not a
// calibrated probability by construction. Consumers persist the distribution
// (llmlog metadata) so calibration can be measured afterwards.

// ErrNoLogprobs is returned when the response carries no top-logprobs: the
// request did not ask (Options.TopLogprobs == 0), the wire cannot report them
// (Ollama /api/chat), or the provider silently ignored the field. Callers
// treat it as "this backend cannot decide" and fall back to a generation
// prompt for the same question.
var ErrNoLogprobs = errors.New("llm: decide: response carries no top-logprobs")

// ErrNoLabelMass is returned when the provider reported logprobs but the
// caller's labels captured less than MinLabelMass of the first token's belief
// — the model answered something else (a thinking tag, prose, a code fence)
// with at most a sliver on the vocabulary. Also a fallback signal: a
// renormalisation of 9 % belief is not a decision.
var ErrNoLabelMass = errors.New("llm: decide: no probability mass on any label")

// MinLabelMass is the share of first-token probability the labels must
// capture for DecideChoice to return a decision. Measured in-vocabulary answers
// carry 0.96–1.00 (goldbench decide axes, mean 0.996); an answer that starts
// with prose or a thinking tag leaves the labels far below 0.5. Half is the
// point where "the model chose among the labels" stops being the majority
// reading of the token.
const MinLabelMass = 0.5

// TokenLogprob is one entry of a top-logprobs list: the token text as the
// provider tokenizes it (leading space included where the tokenizer keeps
// one) and its natural-log probability at that position.
type TokenLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

// FirstTokenLogprobs assembles the read-out list of ONE token position from
// the provider's shape: the sampled token (with its log-probability, nil when
// the provider sent JSON null — how llama.cpp serialises −∞ for a masked
// token) and the listed alternatives. Entries with a nil or positive
// log-probability are dropped rather than decoded as 0 (= probability 1);
// the sampled token is appended once when the alternatives do not list it.
// Shared by the production client and the goldbench client so both read the
// same distribution.
func FirstTokenLogprobs(sampled string, sampledLogprob *float64, alts []TokenLogprob) []TokenLogprob {
	out := make([]TokenLogprob, 0, len(alts)+1)
	seen := false
	for _, a := range alts {
		if a.Logprob > 0 || math.IsNaN(a.Logprob) {
			continue
		}
		out = append(out, a)
		seen = seen || a.Token == sampled
	}
	if !seen && sampledLogprob != nil && *sampledLogprob <= 0 && !math.IsNaN(*sampledLogprob) {
		out = append(out, TokenLogprob{Token: sampled, Logprob: *sampledLogprob})
	}
	return out
}

// DecideOptions is the sampling preset of a decide call: greedy, ONE output
// token, top-20 alternatives. NumPredict is CapLocked so a serving row's
// generous num_predict for the role's other phases cannot inflate the one
// token into a paragraph — the answer is read before decoding anyway, the
// lock only protects the cost.
func DecideOptions() Options {
	return Options{
		Temperature: 0,
		NumPredict:  1,
		CapLocked:   true,
		TopLogprobs: 20,
	}
}

// Decision is the read-out of one decide call.
type Decision struct {
	// Best is the label with the highest probability (ties: first in the
	// caller's label order). Empty only when the call errored.
	Best string
	// Probs maps every label to its probability, renormalised over the
	// labels (they sum to 1). Labels absent from the provider list are 0.
	Probs map[string]float64
	// Mass is the raw probability the labels captured BEFORE renormalisation
	// — the share of the model's first-token belief that went to any label
	// at all. 0.99 means the model answered in the requested vocabulary; a
	// low value means the distribution is a renormalisation of noise.
	Mass float64
	// Confidence is the TypeSafe-style collapse of the distribution's shape,
	// (n·peak − 1)/(n − 1) for n labels: 1 when all mass sits on one label,
	// 0 for the uniform distribution. For n == 1 it equals the peak.
	Confidence float64
}

// DecideChoice reads a distribution over labels from top. Label matching is
// exact after trimming ASCII/Unicode whitespace on the token side (tokenizers
// keep a leading space on word tokens); it is case-sensitive on purpose —
// callers choose labels whose casing the prompt dictates ("Yes", "A").
//
// Probability is exp(logprob), summed when several token spellings map to the
// same label (" A" and "A"). Labels must be non-empty and unique.
func DecideChoice(top []TokenLogprob, labels []string) (Decision, error) {
	if len(labels) == 0 {
		return Decision{}, errors.New("llm: decide: no labels")
	}
	if len(top) == 0 {
		return Decision{}, ErrNoLogprobs
	}
	index := make(map[string]int, len(labels))
	for i, l := range labels {
		if l == "" {
			return Decision{}, fmt.Errorf("llm: decide: empty label at %d", i)
		}
		if _, dup := index[l]; dup {
			return Decision{}, fmt.Errorf("llm: decide: duplicate label %q", l)
		}
		index[l] = i
	}
	raw := make([]float64, len(labels))
	var mass float64
	for _, t := range top {
		i, ok := index[strings.TrimSpace(t.Token)]
		if !ok {
			continue
		}
		if math.IsNaN(t.Logprob) || math.IsInf(t.Logprob, 1) {
			continue
		}
		p := math.Exp(t.Logprob)
		raw[i] += p
		mass += p
	}
	if mass < MinLabelMass {
		return Decision{}, fmt.Errorf("%w (mass %.3f < %.2f)", ErrNoLabelMass, mass, MinLabelMass)
	}
	d := Decision{Probs: make(map[string]float64, len(labels)), Mass: math.Min(mass, 1)}
	best, peak := 0, -1.0
	for i, l := range labels {
		p := raw[i] / mass
		d.Probs[l] = p
		if p > peak {
			best, peak = i, p
		}
	}
	d.Best = labels[best]
	if n := float64(len(labels)); n > 1 {
		d.Confidence = math.Max(0, math.Min(1, (n*peak-1)/(n-1)))
	} else {
		d.Confidence = peak
	}
	return d, nil
}

// Decide is DecideChoice over a ChatResponse — the shape every chain walk
// hands back. A nil response is ErrNoLogprobs.
func Decide(resp *ChatResponse, labels []string) (Decision, error) {
	if resp == nil {
		return Decision{}, ErrNoLogprobs
	}
	return DecideChoice(resp.TopLogprobs, labels)
}

// IsDecideFallback reports whether err is one of the two "this backend/answer
// cannot be read as a decision" signals — the condition under which a caller
// re-asks the same question as a generation prompt. Wire and admission errors
// are NOT fallback: they are the same failures a generation call would have.
func IsDecideFallback(err error) bool {
	return errors.Is(err, ErrNoLogprobs) || errors.Is(err, ErrNoLabelMass)
}
