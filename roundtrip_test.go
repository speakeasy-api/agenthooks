package agenthooks_test

// Round-trip property (DESIGN.md §5.4): for every fixture in the corpus,
// decode → encode(NoDecision) produces the provider-correct no-op, and
// Event.Raw is byte-identical to the input.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/agenthookstest"
)

func TestRoundTripNoOpAndRawFidelity(t *testing.T) {
	providers := []agenthooks.Provider{
		agenthooks.ProviderClaudeCode,
		agenthooks.ProviderCodex,
		agenthooks.ProviderCursor,
		agenthooks.ProviderGemini,
		agenthooks.ProviderOpenCode,
		agenthooks.ProviderKimi,
	}
	quiet := agenthooks.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, p := range providers {
		for name, payload := range agenthookstest.Fixtures(t, agenthookstest.FixtureDir(p)) {
			t.Run(string(p)+"/"+name, func(t *testing.T) {
				var raws [][]byte
				// Round-trip concerns one event's wire behavior: session-level
				// synthesis (prompt backfill) would add a second OnAny delivery.
				r := agenthooks.New(quiet, agenthooks.WithoutDedup(), agenthooks.WithoutBackfill())
				r.OnAny(func(ctx context.Context, e *agenthooks.Event) error {
					raw := make([]byte, len(e.Raw))
					copy(raw, e.Raw)
					raws = append(raws, raw)
					if e.Provider != p {
						t.Errorf("provider = %q, want %q", e.Provider, p)
					}
					if e.NativeName == "" {
						t.Error("native name must always be set")
					}
					return nil
				})
				res := agenthookstest.Invoke(t, r, p, payload)
				agenthookstest.AssertNoOp(t, p, res)
				if len(raws) != 1 {
					t.Fatalf("OnAny should fire exactly once, fired %d times", len(raws))
				}
				if !bytes.Equal(raws[0], payload) {
					t.Errorf("Raw not byte-identical:\n raw: %s\nwant: %s", raws[0], payload)
				}
			})
		}
	}
}
