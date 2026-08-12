// Command hookbin is the subprocess fixture for the client/server
// end-to-end test (clientserver_e2e_test.go): a minimal consumer binary
// whose tool.pre handler denies and appends its process id to
// $HOOKBIN_LOG. Two client invocations answered by the same pid prove the
// decisions came from one long-running auto-spawned server rather than
// two independently spawned servers.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/speakeasy-api/agenthooks"
)

func main() {
	r := agenthooks.New()
	r.OnToolPre(func(ctx context.Context, e *agenthooks.ToolPreEvent) (agenthooks.ToolPreDecision, error) {
		if path := os.Getenv("HOOKBIN_LOG"); path != "" {
			if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
				_ = f.Close()
			}
		}
		return agenthooks.Deny("denied by hookbin"), nil
	})
	agenthooks.Main(r)
}
