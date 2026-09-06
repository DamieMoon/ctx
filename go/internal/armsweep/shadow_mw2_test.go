package armsweep_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/armsweep"
)

// M-W2 non-regression gate (l), the instance-kind gate (design/05 §5 B4b).
//
// The shadow corpus is invisible in the RESULT, not in the INDEXES: its blocks
// need embeddings and tsvectors, and neither idx_embedding_hnsw nor the two GIN
// indexes is partial — so every shadow block spends scan budget on every
// PRODUCTION query. That is why the measurement corpus is built in a restore
// copy, and why the driver must refuse to dump shadow types against an instance
// that does not say it is one. The stamp is read off the MEASURED instance
// (settings key server.instance_kind, restart-only), never off the measuring
// host's environment: an env var on the driver's machine says nothing about the
// instance being measured, and a restart-only key cannot be flipped by a hot
// settings write.

// mw2FakeCtx serves the two endpoints the gate touches and counts the reads.
type mw2FakeCtx struct {
	srv   *httptest.Server
	kind  string
	code  int
	reads atomic.Int32
	// bodies records the /api/query request bodies the driver sent.
	bodies chan string
}

func mw2NewFakeCtx(t *testing.T, kind string) *mw2FakeCtx {
	t.Helper()
	f := &mw2FakeCtx{kind: kind, code: http.StatusOK, bodies: make(chan string, 8)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/settings/"):
			f.reads.Add(1)
			if f.code != http.StatusOK {
				w.WriteHeader(f.code)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "nope"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"setting": map[string]any{"key": strings.TrimPrefix(r.URL.Path, "/api/settings/"), "value": f.kind},
			})
		case r.URL.Path == "/api/query":
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			select {
			case f.bodies <- string(buf):
			default:
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true, "sources": []any{},
				"arm_ranks": map[string]any{"rows": []any{}, "fusion_order": []any{},
					"effective_query": "q", "embed_model": "m",
					"selector": map[string]any{"mode": "ann", "reason": "disabled"}},
			})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *mw2FakeCtx) client() *armsweep.Client {
	return armsweep.NewClient(f.srv.URL, "k", 5*time.Second)
}

// TestMW2InstanceKindGate is gate (l): shadow types against an instance that is
// not stamped as a measure copy is a refusal, and the refusal is a distinct
// error class so a scheduler can tell it from a failed run.
//
// The subject is the PRODUCTION pair — StampInstanceKind reads the label,
// CheckInstanceKind refuses on it — in the order cmd/ctx-armsweep composes them
// (commands.go gateInstance, :202/:206). M-W2 shipped a third function that
// fused the two halves; X-W3a split them because the label belongs on EVERY
// dump and the refusal only on a shadow run, and from then on nothing but this
// test called the fused form. It is gone, and the gate now probes what the
// driver actually runs instead of a parallel implementation of it.
//
// RED before M-W2: `undefined: armsweep.CheckInstanceKind`.
func TestMW2InstanceKindGate(t *testing.T) {
	shadow := []string{"mw2-shadow"}

	// gate mirrors cmd/ctx-armsweep's gateInstance for a non-dry run: read the
	// label off the instance, then refuse on it.
	gate := func(t *testing.T, f *mw2FakeCtx, shadowTypes []string, allowLive bool) (string, error) {
		t.Helper()
		kind, err := armsweep.StampInstanceKind(context.Background(), f.client())
		if err != nil {
			return "", err
		}
		return kind, armsweep.CheckInstanceKind(kind, shadowTypes, allowLive)
	}

	t.Run("live instance refuses", func(t *testing.T) {
		f := mw2NewFakeCtx(t, armsweep.InstanceKindLive)
		kind, err := gate(t, f, shadow, false)
		if !errors.Is(err, armsweep.ErrNotMeasureCopy) {
			t.Fatalf("err = %v, want ErrNotMeasureCopy", err)
		}
		if kind != armsweep.InstanceKindLive {
			t.Errorf("kind = %q, want %q", kind, armsweep.InstanceKindLive)
		}
	})

	t.Run("measure copy passes", func(t *testing.T) {
		f := mw2NewFakeCtx(t, armsweep.InstanceKindMeasureCopy)
		kind, err := gate(t, f, shadow, false)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if kind != armsweep.InstanceKindMeasureCopy {
			t.Errorf("kind = %q, want %q", kind, armsweep.InstanceKindMeasureCopy)
		}
	})

	t.Run("explicit override passes and reports the true kind", func(t *testing.T) {
		f := mw2NewFakeCtx(t, armsweep.InstanceKindLive)
		kind, err := gate(t, f, shadow, true)
		if err != nil {
			t.Fatalf("err = %v, want nil under the override", err)
		}
		// The override buys passage, never a relabel: the stamp keeps saying
		// what the instance said, so the report cannot hide which corpus the
		// numbers came from.
		if kind != armsweep.InstanceKindLive {
			t.Errorf("kind = %q, want the instance's own %q", kind, armsweep.InstanceKindLive)
		}
	})

	t.Run("an unreadable stamp is a refusal, not a pass", func(t *testing.T) {
		f := mw2NewFakeCtx(t, armsweep.InstanceKindMeasureCopy)
		f.code = http.StatusNotFound
		if _, err := gate(t, f, shadow, false); err == nil {
			t.Fatal("err = nil for an instance whose stamp could not be read")
		}
	})

	t.Run("an unknown value is a refusal", func(t *testing.T) {
		f := mw2NewFakeCtx(t, "something-else")
		if _, err := gate(t, f, shadow, false); !errors.Is(err, armsweep.ErrNotMeasureCopy) {
			t.Fatalf("err = %v, want ErrNotMeasureCopy", err)
		}
	})

	t.Run("a silent instance stamps unknown, and unknown is a refusal", func(t *testing.T) {
		// The empty answer is the one case the two halves disagree about: the
		// stamp must never write "" (that byte means "dry run" in a dump), and
		// the refusal must not read "unknown" as a measure copy.
		f := mw2NewFakeCtx(t, "")
		kind, err := gate(t, f, shadow, false)
		if kind != armsweep.InstanceKindUnknown {
			t.Errorf("kind = %q, want %q — an empty answer must not stamp as an empty string", kind, armsweep.InstanceKindUnknown)
		}
		if !errors.Is(err, armsweep.ErrNotMeasureCopy) {
			t.Fatalf("err = %v, want ErrNotMeasureCopy", err)
		}
	})

	t.Run("without shadow types the refusal does not fire", func(t *testing.T) {
		// Since X-W3a the label is read on EVERY non-dry run — an ordinary dump
		// carries the instance's kind too (that is the F-32 campaign rule) and
		// only the REFUSAL is reserved for a shadow run.
		f := mw2NewFakeCtx(t, armsweep.InstanceKindLive)
		kind, err := gate(t, f, nil, false)
		if err != nil {
			t.Fatalf("err = %v, want nil for an ordinary dump", err)
		}
		if kind != armsweep.InstanceKindLive {
			t.Errorf("kind = %q, want the instance's own %q — an ordinary dump is stamped too", kind, armsweep.InstanceKindLive)
		}
		if n := f.reads.Load(); n != 1 {
			t.Errorf("the label was read %d times, want exactly 1 per run", n)
		}
	})
}

// TestMW2RunnerSendsShadowTypes pins the wire half: the driver's measurement
// request carries the shadow list, and an ordinary run still carries no such
// key at all (so a pre-M-W2 instance sees the byte-identical body it always
// saw).
//
// RED before M-W2: `unknown field ShadowTypes in struct literal`.
func TestMW2RunnerSendsShadowTypes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shadow []string
		want   string
		absent bool
	}{
		{name: "with shadow types", shadow: []string{"mw2-shadow", "mw2-other"},
			want: `"shadow_types":["mw2-shadow","mw2-other"]`},
		{name: "without", absent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mw2NewFakeCtx(t, armsweep.InstanceKindMeasureCopy)
			req := armsweep.QueryRequest{Query: "q", Synthesize: false, ArmRanks: true, ShadowTypes: tc.shadow}
			if _, err := f.client().Measure(context.Background(), req); err != nil {
				t.Fatalf("measure: %v", err)
			}
			body := <-f.bodies
			switch {
			case tc.absent && strings.Contains(body, "shadow_types"):
				t.Errorf("an ordinary measurement body carries shadow_types: %s", body)
			case !tc.absent && !strings.Contains(body, tc.want):
				t.Errorf("body %s does not carry %s", body, tc.want)
			}
		})
	}
}

// TestMW2StampRecordsTheInstance pins that the stamp — and with it every report
// built from it — says which instance kind produced the numbers and whether the
// override was used. Without those two fields the rule "all dumps of one
// campaign come from the same instance" (§5 B4b, F-32) would be a convention
// instead of something a later `compare` can gate on.
func TestMW2StampRecordsTheInstance(t *testing.T) {
	stamp := armsweep.DumpStamp{
		InstanceKind:      armsweep.InstanceKindMeasureCopy,
		ShadowTypes:       []string{"mw2-shadow"},
		AllowLiveInstance: true,
	}
	b, err := json.Marshal(stamp)
	if err != nil {
		t.Fatalf("marshal stamp: %v", err)
	}
	for _, want := range []string{
		`"instance_kind":"measure-copy"`,
		`"shadow_types":["mw2-shadow"]`,
		`"allow_live_instance":true`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("stamp JSON lacks %s: %s", want, b)
		}
	}
}
