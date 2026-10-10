package chat

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// TestChatOptionsCarriesModelMapEffort pins the review finding that the
// harness bypasses applyModelParams: a chat row's params.effort must still
// reach Options.Extra, the slot the anthropic stream wire reads — otherwise
// docs/api.md's "model_map params.effort beats extra_body" holds on the
// non-stream path only.
func TestChatOptionsCarriesModelMapEffort(t *testing.T) {
	e := &Engine{cfg: Config{}.withDefaults(), now: time.Now}
	b := backends.Backend{ModelMap: map[string]backends.ModelSpec{
		"chat": {Model: "claude-opus-5-5", Params: map[string]any{"effort": "xhigh", "temperature": 0.2}},
	}}
	opts := e.chatOptions(b, 100)
	if opts.Extra["effort"] != "xhigh" {
		t.Errorf("Options.Extra = %v, want effort xhigh from model_map params", opts.Extra)
	}
	if opts.Temperature != 0.2 || opts.NumPredict != 100 {
		t.Errorf("temperature/num_predict = %v/%d, want 0.2/100", opts.Temperature, opts.NumPredict)
	}

	// Without the param the slot stays nil — the openai wire's extra_body
	// merge and the anthropic default are untouched.
	plain := e.chatOptions(backends.Backend{ModelMap: map[string]backends.ModelSpec{"chat": {Model: "m"}}}, 100)
	if plain.Extra != nil {
		t.Errorf("Options.Extra = %v, want nil without an effort param", plain.Extra)
	}
}
