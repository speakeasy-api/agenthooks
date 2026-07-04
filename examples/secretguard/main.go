// Command secretguard is an example agenthooks consumer: it scans every tool
// call's input for credential-shaped strings before execution. On a hit it
// forces the provider's confirmation prompt, so the user can accept the risk
// and continue — or reject and block the call. On providers/events without a
// confirmation prompt (Codex, Cursor's generic preToolUse, OpenCode) the call
// is blocked outright, because letting a detected secret through unchallenged
// is the one outcome this hook exists to prevent.
//
// Install it for a project with the install package, or by hand, e.g. Claude
// Code .claude/settings.json:
//
//	{"hooks": {"PreToolUse": [{"matcher": "", "hooks": [
//	  {"type": "command", "command": "secretguard agenthooks run --provider=claude-code", "timeout": 10}
//	]}]}}
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/speakeasy-api/agenthooks"
)

func main() {
	agenthooks.Main(buildRunner())
}

// buildRunner is separated from main so the example is testable in-process.
func buildRunner() *agenthooks.Runner {
	r := agenthooks.New(
		agenthooks.WithPolicy(agenthooks.Policy{
			// If this hook crashes or times out, block rather than leak.
			Fail: agenthooks.FailClosed,
			// Belt and braces: should an AskUser ever reach a provider that
			// can't prompt, degrade it to a deny, never to a pass-through.
			Unsupported: agenthooks.Degrade,
			AskFallback: agenthooks.FallbackDeny,
		}),
	)

	r.OnToolPre(func(ctx context.Context, e *agenthooks.ToolPreEvent) (agenthooks.ToolPreDecision, error) {
		findings := Scan(e.Tool.Input)
		if len(findings) == 0 {
			return agenthooks.NoDecision(), nil
		}

		found := describe(findings)
		agenthooks.Logger(ctx).Warn("secretguard: secrets detected in tool input",
			"tool", e.Tool.Name, "findings", len(findings))

		if !e.Can(agenthooks.CapAsk) {
			// No confirmation prompt exists here (Policy.AskFallback would
			// catch this too; the explicit branch keeps the example honest
			// about what actually happens on each provider).
			return agenthooks.Deny(fmt.Sprintf(
				"tool call blocked: input contains credential-shaped strings: %s (this provider offers no confirmation prompt to accept the risk)",
				found,
			)), nil
		}
		return agenthooks.AskUser(fmt.Sprintf(
			"tool call input contains credential-shaped strings: %s. Approve to accept the risk and continue; reject to block the call.",
			found,
		)).WithSystemMessage("secretguard: " + found), nil
	})

	return r
}

func describe(findings []Finding) string {
	parts := make([]string, len(findings))
	for i, f := range findings {
		parts[i] = fmt.Sprintf("%s (%s)", f.Rule, f.Masked)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
