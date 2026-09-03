// Command recorder is the consumer binary the e2e suite installs as a hook
// into real coding agents. It records every event the library delivers as a
// JSONL line and optionally denies a canonical tool class, so tests can
// assert both the decode path (what arrived, how it normalized) and the
// encode path (a deny actually blocks the tool in the real agent).
//
// Configuration is read from <executable>.e2e.json — not from env vars or
// argv — so it works even when a provider strips the hook process
// environment and without disturbing the library-owned argv contract.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"

	"github.com/speakeasy-api/agenthooks"
)

type config struct {
	// Out is the JSONL sink; one line per delivered event.
	Out string `json:"out"`
	// Deny names a CanonicalTool class to deny on tool.pre ("" allows all).
	Deny string `json:"deny,omitempty"`
	// RewriteCommand replaces shell tool arguments and explicitly allows the call.
	RewriteCommand string `json:"rewrite_command,omitempty"`
	// ContinueInstruction is returned from the first agent.stop as
	// ContinueWith, so a test can assert the agent really kept working. It
	// fires at most once per sink: providers that never report their own
	// continuation guard (stop_hook_active) would otherwise loop forever,
	// and the library cap only trips on a reported LoopCount.
	ContinueInstruction string `json:"continue_instruction,omitempty"`
	// PromptContext is added through the provider's prompt-submitted decision
	// channel. E2E tests use it to prove the native runtime honors context, not
	// merely that the codec serialized a response.
	PromptContext string `json:"prompt_context,omitempty"`
}

// record is one JSONL line. Kind "tool.pre" lines are emitted twice: once by
// OnAny (raw envelope) and once by the typed handler with the normalized
// tool fields (Typed=true), so tests can assert the normalization too.
type record struct {
	Typed      bool            `json:"typed,omitempty"`
	Backfilled bool            `json:"backfilled,omitempty"`
	TimeMS     int64           `json:"time_ms,omitempty"`
	Provider   string          `json:"provider"`
	Variant    string          `json:"variant,omitempty"`
	Native     string          `json:"native"`
	Kind       string          `json:"kind"`
	Session    string          `json:"session_id,omitempty"`
	TurnID     string          `json:"turn_id,omitempty"`
	CWD        string          `json:"cwd,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	Canonical  string          `json:"canonical,omitempty"`
	ToolInput  json.RawMessage `json:"tool_input,omitempty"`
	Prompt     string          `json:"prompt,omitempty"`
	Denied     bool            `json:"denied,omitempty"`
	Rewritten  bool            `json:"rewritten,omitempty"`
	Continued  bool            `json:"continued,omitempty"`
	// PrevContinued/LoopCount are the provider's own continuation guard as
	// the library unified it; both are recorded rather than asserted, since
	// whether a provider reports one at all is what the test measures.
	PrevContinued bool            `json:"prev_continued,omitempty"`
	LoopCount     int             `json:"loop_count,omitempty"`
	Raw           json.RawMessage `json:"raw,omitempty"`
}

func main() {
	cfg := loadConfig()

	r := agenthooks.New()
	r.OnAny(func(_ context.Context, e *agenthooks.Event) error {
		appendRecord(cfg.Out, record{
			Backfilled: e.Backfilled,
			TimeMS:     e.Time.UnixMilli(),
			Provider:   string(e.Provider),
			Variant:    string(e.Variant),
			Native:     e.NativeName,
			Kind:       string(e.Kind),
			Session:    e.Session.ID,
			TurnID:     e.Session.TurnID,
			CWD:        e.Session.CWD,
			Raw:        e.Raw,
		})
		return nil
	})
	r.OnPromptSubmitted(func(_ context.Context, e *agenthooks.PromptEvent) (agenthooks.PromptDecision, error) {
		appendRecord(cfg.Out, record{
			Typed:      true,
			Backfilled: e.Backfilled,
			TimeMS:     e.Time.UnixMilli(),
			Provider:   string(e.Provider),
			Native:     e.NativeName,
			Kind:       string(e.Kind),
			Session:    e.Session.ID,
			TurnID:     e.Session.TurnID,
			Prompt:     e.Prompt,
		})
		decision := agenthooks.AcceptPrompt()
		if cfg.PromptContext != "" {
			decision = decision.WithContext(cfg.PromptContext)
		}
		return decision, nil
	})
	r.OnToolPre(func(_ context.Context, e *agenthooks.ToolPreEvent) (agenthooks.ToolPreDecision, error) {
		denied := cfg.Deny != "" && string(e.Tool.Canonical) == cfg.Deny
		rewritten := cfg.RewriteCommand != "" && e.Tool.Canonical == agenthooks.ToolShell
		appendRecord(cfg.Out, record{
			Typed:     true,
			TimeMS:    e.Time.UnixMilli(),
			Provider:  string(e.Provider),
			Variant:   string(e.Variant),
			Native:    e.NativeName,
			Kind:      string(e.Kind),
			Session:   e.Session.ID,
			TurnID:    e.Session.TurnID,
			Tool:      e.Tool.Name,
			Canonical: string(e.Tool.Canonical),
			ToolInput: e.Tool.Input,
			Denied:    denied,
			Rewritten: rewritten,
		})
		if denied {
			return agenthooks.Deny("blocked by agenthooks e2e"), nil
		}
		if rewritten {
			return agenthooks.Allow().WithUpdatedInput(map[string]any{"command": cfg.RewriteCommand}), nil
		}
		return agenthooks.NoDecision(), nil
	})
	r.OnStop(func(_ context.Context, e *agenthooks.StopEvent) (agenthooks.StopDecision, error) {
		cont := cfg.ContinueInstruction != "" && !e.PreviouslyContinued && !alreadyContinued(cfg.Out)
		appendRecord(cfg.Out, record{
			Typed:         true,
			TimeMS:        e.Time.UnixMilli(),
			Provider:      string(e.Provider),
			Variant:       string(e.Variant),
			Native:        e.NativeName,
			Kind:          string(e.Kind),
			Session:       e.Session.ID,
			TurnID:        e.Session.TurnID,
			Continued:     cont,
			PrevContinued: e.PreviouslyContinued,
			LoopCount:     e.LoopCount,
		})
		if cont {
			return agenthooks.ContinueWith(cfg.ContinueInstruction), nil
		}
		return agenthooks.Finish(), nil
	})

	agenthooks.Main(r)
}

func loadConfig() config {
	var cfg config
	exe, err := os.Executable()
	if err != nil {
		return cfg
	}
	data, err := os.ReadFile(exe + ".e2e.json")
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

// alreadyContinued reports whether an earlier hook process for this sink
// already returned a continuation. Each hook firing is its own process, so
// the sink is the only shared state available to bound the loop.
func alreadyContinued(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && bytes.Contains(data, []byte(`"continued":true`))
}

func appendRecord(path string, rec record) {
	if path == "" {
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}
