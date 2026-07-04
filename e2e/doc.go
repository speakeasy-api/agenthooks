// Package e2e exercises agenthooks against real, locally installed coding
// agents: it renders hook configuration with the install package, points it
// at a recorder binary built from testdata/recorder, drives one headless
// agent turn, and asserts on the events that actually arrived and on
// decision enforcement (a deny must block the tool).
//
// The suite is opt-in — it spends real model tokens and needs authenticated
// local agent CLIs:
//
//	AGENTHOOKS_E2E=1 go test ./e2e -v
//
// Tests for agents that are not installed (or when AGENTHOOKS_E2E is unset)
// skip, so the package is inert in CI.
package e2e
