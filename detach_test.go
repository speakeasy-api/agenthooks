package agenthooks

import (
	"reflect"
	"testing"
)

func TestStripAsyncFlag(t *testing.T) {
	rest, ok := stripAsyncFlag([]string{"agenthooks", "run", "--provider=codex", "--async"})
	if !ok || !reflect.DeepEqual(rest, []string{"agenthooks", "run", "--provider=codex"}) {
		t.Errorf("async flag must be detected and stripped: %v %v", rest, ok)
	}
	rest, ok = stripAsyncFlag([]string{"agenthooks", "run", "--provider=codex"})
	if ok || len(rest) != 3 {
		t.Errorf("no flag means no detach: %v %v", rest, ok)
	}
}

func TestClaudeMCPWarmCWD(t *testing.T) {
	cwd, ok := claudeMCPWarmCWD([]string{"agenthooks", "run", claudeMCPWarmFlag + "=/work/project"})
	if !ok || cwd != "/work/project" {
		t.Fatalf("warm cwd = %q, %v", cwd, ok)
	}
	if _, ok := claudeMCPWarmCWD([]string{"agenthooks", "run"}); ok {
		t.Fatal("ordinary invocation detected as MCP warm worker")
	}
}

// The runner itself must tolerate --async unseen (an old library driven by a
// newer generated config runs the hook synchronously instead of erroring).
func TestParseArgsToleratesAsync(t *testing.T) {
	inv, err := parseArgs([]string{"agenthooks", "run", "--provider=codex", "--async"})
	if err != nil || inv.provider != ProviderCodex {
		t.Errorf("--async must parse as a tolerated unknown: %+v %v", inv, err)
	}
}
