package agenthooks

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/speakeasy-api/agenthooks/internal/transcriptio"
)

// Some providers skip events in some modes: Kimi's and Cursor's
// non-interactive print modes never fire their prompt-submitted hooks
// (quirks #30, #31), while later events for the same turn fire normally. The
// runner backfills the miss best-effort: when an event that implies a prompt
// arrives for a session where no prompt.submitted was delivered, it
// synthesizes one, delivered before the triggering event.
//
// A backfilled event is reporting-only. Backfilled is true, Raw is nil (the
// provider sent no payload — nothing is fabricated), Can() reports no
// capabilities, and any decision the handler returns is discarded: the
// moment to gate the prompt has already passed. The prompt text is recovered
// best-effort — from Kimi's session store (wire.jsonl); on Cursor from the
// session transcript every hook payload names, falling back to the agent
// process's own argv (promptargv.go). PromptEvent.Prompt is empty when
// recovery fails.
//
// Reporting-only is a hard boundary for the backfilled event itself, but not
// for policy: the backfill dispatches in the same process immediately before
// the triggering event, so state a PromptSubmitted handler records is visible
// to the triggering event's handler — which can decide. Consumers that need
// to gate on a recovered prompt deny the triggering tool call there.
//
// Tracking uses best-effort on-disk markers keyed by session and the
// recovered prompt's fingerprint (each hook firing is a separate process).
// Keying on the fingerprint makes resumed headless turns work: a resume
// (`-p --resume`) drops the prompt event again, and the newly recovered text
// claims a fresh marker while mid-turn repeats of already-reported text stay
// suppressed. Two blind spots follow from fingerprinting: a turn whose text
// repeats an earlier turn of the same session is indistinguishable from a
// re-delivery and stays suppressed, and a crash between claiming and
// dispatching loses that one turn. Any filesystem error means "do not
// backfill" — a missed backfill is noise-free, a duplicate is not.

const backfillTTL = 24 * time.Hour

// promptImplied reports whether an event kind can only occur after the user
// submitted a prompt.
func promptImplied(k EventKind) bool {
	switch k {
	case KindToolPre, KindToolPost, KindToolError, KindPermission,
		KindStop, KindSubagentStart, KindSubagentStop:
		return true
	}
	return false
}

// promptMarkerDir holds one marker file per reported prompt, named
// <sessionKey>.<promptFingerprint>.
func (r *Runner) promptMarkerDir() string {
	return filepath.Join(r.stateDir(), "agenthooks-promptseen")
}

func promptSessionKey(base *Event) string {
	if base.Session.ID == "" {
		return "" // no key to track under
	}
	key := sha256.Sum256([]byte(string(base.Provider) + "|" + base.Session.ID))
	return hex.EncodeToString(key[:])[:16]
}

func promptFingerprint(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}

// sessionHasPromptMarker reports whether any prompt was already reported for
// the session, regardless of text.
func sessionHasPromptMarker(dir, sessionKey string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), sessionKey+".") {
			return true
		}
	}
	return false
}

// notePromptSeen marks the session as having received a real prompt event
// with the given text, so recovery of the same text never backfills. The
// transcript/store parsers normalize to exactly what the real event would
// have carried, keeping the fingerprints aligned.
func (r *Runner) notePromptSeen(base *Event, prompt string) {
	sess := promptSessionKey(base)
	if sess == "" {
		return
	}
	dir := r.promptMarkerDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	cleanupStaleBackfill(dir)
	if f, err := os.OpenFile(filepath.Join(dir, sess+"."+promptFingerprint(prompt)), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_ = f.Close()
	}
}

// maybeBackfillPrompt synthesizes a reporting-only prompt.submitted if the
// recovered prompt text was not reported for this session yet. The marker is
// claimed with O_EXCL so concurrent sibling processes backfill at most once
// per prompt.
func (r *Runner) maybeBackfillPrompt(ctx context.Context, base *Event) {
	sess := promptSessionKey(base)
	if sess == "" {
		return
	}
	dir := r.promptMarkerDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	cleanupStaleBackfill(dir)
	recovered := recoverPromptText(base)
	if recovered == "" && sessionHasPromptMarker(dir, sess) {
		// Recovery failed for a session that already reported a prompt:
		// indistinguishable from a mid-turn repeat, so skip.
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, sess+"."+promptFingerprint(recovered)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return // this prompt already reported (or FS error: skip, never duplicate)
	}
	_ = f.Close()

	pe := &PromptEvent{Event: Event{
		Provider:            base.Provider,
		Variant:             base.Variant,
		Kind:                KindPromptSubmitted,
		Time:                r.now(),
		Session:             base.Session,
		Agent:               base.Agent,
		DetectionConfidence: base.DetectionConfidence,
		Backfilled:          true,
	}}
	pe.Prompt = recovered
	core, err := r.dispatch(ctx, pe)
	if err != nil {
		r.logger.Warn("agenthooks: handler error on backfilled prompt.submitted (reporting-only, ignored)", "error", err)
	}
	if core.kind != decNoDecision && core.kind != decAcceptPrompt {
		r.logger.Debug("agenthooks: decision on backfilled prompt.submitted discarded (event is reporting-only)")
	}
}

// recoverPromptText best-effort recovers the submitted prompt. Only the
// providers that need backfilling are covered; any failure (missing store,
// format drift, unreadable process tree) yields "" — recovery is advisory,
// never authoritative.
func recoverPromptText(base *Event) string {
	switch base.Provider {
	case ProviderKimi:
		return kimiStoredPrompt(base.Session.ID)
	case ProviderCursor:
		if p := cursorTranscriptPrompt(base.Session.TranscriptPath); p != "" {
			return p
		}
		return cursorArgvPrompt()
	}
	return ""
}

// cursorTranscriptPrompt returns the current turn's prompt from the session
// transcript named in every cursor hook payload: the last user entry with
// text — tool results are user-role but carry no text blocks, and Cursor
// writes the user line before firing the turn's tool events, so it is
// present by the first event that triggers a backfill. The parser strips the
// transcript's <user_query> framing, so the text matches what
// beforeSubmitPrompt would have reported.
func cursorTranscriptPrompt(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	entries, err := transcriptio.Read(string(ProviderCursor), f)
	if err != nil {
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role == "user" && entries[i].Text != "" {
			return entries[i].Text
		}
	}
	return ""
}

// kimiStoredPrompt resolves the session's wire.jsonl via
// $KIMI_CODE_HOME/session_index.jsonl and returns the latest turn.prompt.
func kimiStoredPrompt(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	home := os.Getenv("KIMI_CODE_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(h, ".kimi-code")
	}
	idx, err := os.Open(filepath.Join(home, "session_index.jsonl"))
	if err != nil {
		return ""
	}
	defer idx.Close()
	sessionDir := kimiSessionDir(idx, sessionID)
	if sessionDir == "" {
		return ""
	}
	wire, err := os.Open(filepath.Join(sessionDir, "agents", "main", "wire.jsonl"))
	if err != nil {
		return ""
	}
	defer wire.Close()
	return kimiLatestTurnPrompt(wire)
}

func kimiSessionDir(index io.Reader, sessionID string) string {
	sc := bufio.NewScanner(index)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	dir := ""
	for sc.Scan() {
		var line struct {
			SessionID  string `json:"sessionId"`
			SessionDir string `json:"sessionDir"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.SessionID == sessionID {
			dir = line.SessionDir // last entry wins
		}
	}
	return dir
}

func kimiLatestTurnPrompt(wire io.Reader) string {
	sc := bufio.NewScanner(wire)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	prompt := ""
	for sc.Scan() {
		var line struct {
			Type  string `json:"type"`
			Input []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.Type != "turn.prompt" {
			continue
		}
		var parts []string
		for _, in := range line.Input {
			if in.Text != "" {
				parts = append(parts, in.Text)
			}
		}
		if len(parts) > 0 {
			prompt = strings.Join(parts, "\n") // last turn wins
		}
	}
	return prompt
}

func cleanupStaleBackfill(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) < 64 {
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > backfillTTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
