package agenthooks

import "testing"

// Generated configs place consumer-binary flags ahead of the sentinel
// ("mybinary --config=x agenthooks serve --provider=opencode"); the mode
// keyword must still be recognized there.
func TestParseArgsConsumerFlagsBeforeSentinel(t *testing.T) {
	t.Parallel()
	inv, err := parseArgs([]string{"--config=/etc/consumer.json", "agenthooks", "serve", "--provider=opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.mode != "serve" {
		t.Errorf("mode = %q, want %q", inv.mode, "serve")
	}
	if inv.provider != ProviderOpenCode {
		t.Errorf("provider = %q, want %q", inv.provider, ProviderOpenCode)
	}
	if inv.payload != "" {
		t.Errorf("payload = %q, want empty", inv.payload)
	}
}

func TestParseArgsNoSentinel(t *testing.T) {
	t.Parallel()
	inv, err := parseArgs([]string{"--provider=claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.mode != "run" {
		t.Errorf("mode = %q, want %q", inv.mode, "run")
	}
	if inv.provider != ProviderClaudeCode {
		t.Errorf("provider = %q, want %q", inv.provider, ProviderClaudeCode)
	}
}
