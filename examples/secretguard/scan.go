package main

import (
	"encoding/json"
	"regexp"
)

// Finding is one detected credential-shaped string. The value itself is never
// carried around — only a masked preview safe to show in prompts and logs.
type Finding struct {
	Rule   string
	Masked string
}

type rule struct {
	name string
	re   *regexp.Regexp
}

// Order matters where prefixes overlap (Anthropic keys are a subset of the
// OpenAI sk- shape, so they must match first).
var rules = []rule{
	{"AWS access key ID", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\bgh[opusr]_[A-Za-z0-9]{36,}\b`)},
	{"GitHub fine-grained PAT", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{"Stripe live key", regexp.MustCompile(`\b[sr]k_live_[A-Za-z0-9]{16,}\b`)},
	{"Anthropic API key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9-]{20,}\b`)},
	{"OpenAI API key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	{"assigned secret literal", regexp.MustCompile(`(?i)(?:api[_-]?key|secret|token|password|passwd)["']?\s*[:=]\s*["'][^"']{8,}["']`)},
}

// Scan walks every string value in a tool-input JSON object (ToolCall.Input
// is always an object) and reports credential-shaped matches, deduplicated by
// rule. Scanning decoded values rather than the raw JSON text keeps escape
// sequences from hiding or splitting matches.
func Scan(input json.RawMessage) []Finding {
	var decoded any
	if err := json.Unmarshal(input, &decoded); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var findings []Finding
	walkStrings(decoded, func(s string) {
		for _, r := range rules {
			if seen[r.name] {
				continue
			}
			if m := r.re.FindString(s); m != "" {
				seen[r.name] = true
				findings = append(findings, Finding{Rule: r.name, Masked: mask(m)})
			}
		}
	})
	return findings
}

func walkStrings(v any, visit func(string)) {
	switch t := v.(type) {
	case string:
		visit(t)
	case map[string]any:
		for _, val := range t {
			walkStrings(val, visit)
		}
	case []any:
		for _, val := range t {
			walkStrings(val, visit)
		}
	}
}

// mask keeps just enough of the match to be recognizable in a prompt.
func mask(s string) string {
	if len(s) <= 12 {
		return s[:4] + "…"
	}
	return s[:6] + "…" + s[len(s)-4:]
}
