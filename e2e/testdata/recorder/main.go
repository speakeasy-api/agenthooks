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
	CWD        string          `json:"cwd,omitempty"`
	Tool       string          `json:"tool,omitempty"`
	Canonical  string          `json:"canonical,omitempty"`
	ToolInput  json.RawMessage `json:"tool_input,omitempty"`
	Prompt     string          `json:"prompt,omitempty"`
	Denied     bool            `json:"denied,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
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
			Prompt:     e.Prompt,
		})
		return agenthooks.AcceptPrompt(), nil
	})
	r.OnToolPre(func(_ context.Context, e *agenthooks.ToolPreEvent) (agenthooks.ToolPreDecision, error) {
		denied := cfg.Deny != "" && string(e.Tool.Canonical) == cfg.Deny
		appendRecord(cfg.Out, record{
			Typed:     true,
			TimeMS:    e.Time.UnixMilli(),
			Provider:  string(e.Provider),
			Variant:   string(e.Variant),
			Native:    e.NativeName,
			Kind:      string(e.Kind),
			Session:   e.Session.ID,
			Tool:      e.Tool.Name,
			Canonical: string(e.Tool.Canonical),
			ToolInput: e.Tool.Input,
			Denied:    denied,
		})
		if denied {
			return agenthooks.Deny("blocked by agenthooks e2e"), nil
		}
		return agenthooks.NoDecision(), nil
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
