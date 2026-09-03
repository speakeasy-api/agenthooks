package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/install"
)

// TestMoltisEventsAndDecisions drives the real Moltis Gateway against an
// explicitly configured local OpenAI-compatible model. One isolated Gateway
// proves all three mutable surfaces end to end: prompt context reaches stored
// history, Deny prevents exec, and updated input replaces the command Moltis
// executes. The user's live config, data directory, and Gateway are untouched.
func TestMoltisEventsAndDecisions(t *testing.T) {
	bin := requireE2E(t, "moltis")
	baseURL := strings.TrimRight(os.Getenv("AGENTHOOKS_MOLTIS_BASE_URL"), "/")
	rawModel := os.Getenv("AGENTHOOKS_MOLTIS_MODEL")
	if baseURL == "" || rawModel == "" {
		t.Skip("set AGENTHOOKS_MOLTIS_BASE_URL and AGENTHOOKS_MOLTIS_MODEL for the isolated Moltis E2E")
	}
	if strings.ContainsAny(baseURL+rawModel, "\n\r\"") {
		t.Fatal("Moltis E2E URL/model contains a character unsafe for the generated TOML fixture")
	}

	proj := t.TempDir()
	configDir := t.TempDir()
	dataDir := t.TempDir()
	rec := newRecorderWithConfig(t, recorderConfig{
		PromptContext: "Portable context marker: AGENTHOOKS_SHARED_CONTEXT_OK",
	})
	installHooks(t, rec, agenthooks.ProviderMoltis, install.ScopeUser, dataDir)
	installMoltisNativeObservers(t, rec, dataDir, "MessageSending", "MessageSent")

	port := unusedLocalPort(t)
	writeMoltisE2EConfig(t, configDir, port, baseURL, rawModel)
	logPath, stop := startMoltisGateway(t, bin, proj, configDir, dataDir, port)
	defer stop()
	client := &http.Client{Timeout: 10 * time.Second}
	waitMoltis(t, 30*time.Second, func() bool {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, "Gateway health", logPath)

	model := "lmstudio::" + rawModel
	allowed := filepath.Join(proj, "allowed-marker.txt")
	postGraphQL(t, client, port, map[string]any{
		"message": portableContextShellPrompt(allowed),
		"session": "agenthooks-e2e-allow",
		"model":   model,
	})
	waitMoltis(t, 2*time.Minute, func() bool {
		return fileExists(allowed) && fileContains(rec.Events, `"native":"AfterToolCall"`)
	}, "allowed tool.post", logPath)
	waitMoltis(t, 2*time.Minute, func() bool {
		return moltisHistoryContains(client, port, "agenthooks-e2e-allow", "assistant", "AGENTHOOKS_SHARED_CONTEXT_OK")
	}, "prompt context in the model response", logPath)
	waitMoltis(t, 30*time.Second, func() bool {
		return fileContains(rec.Events, `"kind":"agent.stop"`)
	}, "AgentEnd hook", logPath)
	waitMoltis(t, 30*time.Second, func() bool {
		return fileContains(rec.Events, `"native":"MessageSending"`) &&
			fileContains(rec.Events, `"native":"MessageSent"`)
	}, "outbound message lifecycle hooks", logPath)
	evs := rec.events(t)
	requireKinds(t, evs, agenthooks.KindPromptSubmitted, agenthooks.KindToolPre, agenthooks.KindToolPost, agenthooks.KindStop)
	if !hasCanonicalTool(evs, agenthooks.ToolShell) {
		t.Fatalf("Moltis tool did not normalize as shell:\n%s", summarize(evs))
	}
	requireStableMoltisToolID(t, evs)
	requireMoltisTurnCorrelation(t, evs)
	requireMoltisMessageLifecycle(t, evs)

	directDenyRec := recorder{Bin: rec.Bin, Events: filepath.Join(filepath.Dir(rec.Events), "direct-deny-events.jsonl")}
	setRecorderConfig(t, directDenyRec, recorderConfig{Deny: string(agenthooks.ToolShell)})
	directDenied := filepath.Join(proj, "direct-denied-marker.txt")
	postGraphQL(t, client, port, map[string]any{
		"message": "/sh touch " + directDenied,
		"session": "agenthooks-e2e-direct-deny",
		"model":   model,
	})
	waitMoltis(t, 30*time.Second, func() bool {
		history, err := moltisHistory(client, port, "agenthooks-e2e-direct-deny")
		return err == nil && fileContains(directDenyRec.Events, `"denied":true`) &&
			bytes.Contains(history, []byte("blocked by hook: blocked by agenthooks e2e"))
	}, "direct /sh denial", logPath)
	if fileExists(directDenied) {
		t.Fatal("Moltis direct /sh bypassed the portable tool hook")
	}

	denyRec := recorder{Bin: rec.Bin, Events: filepath.Join(filepath.Dir(rec.Events), "deny-events.jsonl")}
	setRecorderConfig(t, denyRec, recorderConfig{Deny: string(agenthooks.ToolShell)})
	denied := filepath.Join(proj, "denied-marker.txt")
	postGraphQL(t, client, port, map[string]any{
		"message": "Use the exec tool once to run exactly `touch " + denied + "`. If blocked, stop without another method.",
		"session": "agenthooks-e2e-deny",
		"model":   model,
	})
	waitMoltis(t, 2*time.Minute, func() bool {
		history, err := moltisHistory(client, port, "agenthooks-e2e-deny")
		return err == nil && fileContains(denyRec.Events, `"denied":true`) &&
			bytes.Contains(history, []byte("blocked by hook: blocked by agenthooks e2e"))
	}, "denied tool result", logPath)
	if fileExists(denied) {
		t.Fatal("Moltis created the marker despite the portable Deny decision")
	}

	rewriteRec := recorder{Bin: rec.Bin, Events: filepath.Join(filepath.Dir(rec.Events), "rewrite-events.jsonl")}
	rewritten := filepath.Join(proj, "rewritten-marker.txt")
	setRecorderConfig(t, rewriteRec, recorderConfig{RewriteCommand: "touch " + rewritten})
	original := filepath.Join(proj, "original-marker.txt")
	postGraphQL(t, client, port, map[string]any{
		"message": "Use the exec tool once to run exactly `touch " + original + "`, then stop.",
		"session": "agenthooks-e2e-rewrite",
		"model":   model,
	})
	waitMoltis(t, 2*time.Minute, func() bool {
		return fileExists(rewritten) && fileContains(rewriteRec.Events, `"rewritten":true`)
	}, "rewritten tool input", logPath)
	if fileExists(original) {
		t.Fatal("Moltis ran the original command instead of the portable input rewrite")
	}
}

func installMoltisNativeObservers(t *testing.T, rec recorder, dataDir string, events ...string) {
	t.Helper()
	for _, event := range events {
		dir := filepath.Join(dataDir, "hooks", "agenthooks-e2e-"+strings.ToLower(event))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		command := fmt.Sprintf("%q agenthooks run --provider=moltis --timeout=30s", rec.Bin)
		content := fmt.Sprintf(`+++
name = %q
description = "Native Moltis lifecycle observer for agenthooks E2E."
events = [%q]
command = %q
timeout = 30
+++
`, "agenthooks-e2e:"+event, event, command)
		if err := os.WriteFile(filepath.Join(dir, "HOOK.md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func unusedLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func writeMoltisE2EConfig(t *testing.T, dir string, port int, baseURL, model string) {
	t.Helper()
	content := fmt.Sprintf(`[server]
bind = "127.0.0.1"
port = %d
terminal_enabled = false

[auth]
disabled = true
vault_enabled = false

[graphql]
enabled = true

[tls]
enabled = false

[providers]
offered = ["lmstudio"]

[providers.lmstudio]
enabled = true
base_url = %s
models = [%s]
fetch_models = false
tool_mode = "native"

[chat]
priority_models = [%s]
auto_title = false

[memory]
style = "off"
agent_write_mode = "off"
disable_rag = true
session_export = "off"

[tools]
agent_timeout_secs = 180
agent_max_iterations = 5
agent_max_auto_continues = 0

[tools.exec]
approval_mode = "never"
security_level = "permissive"

[tools.exec.sandbox]
mode = "off"
`, port, strconv.Quote(baseURL), strconv.Quote(model), strconv.Quote("lmstudio::"+model))
	if err := os.WriteFile(filepath.Join(dir, "moltis.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startMoltisGateway(t *testing.T, bin, proj, configDir, dataDir string, port int) (string, func()) {
	t.Helper()
	logPath := filepath.Join(dataDir, "gateway.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin,
		"--log-level", "warn",
		"--config-dir", configDir,
		"--data-dir", dataDir,
		"--bind", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"gateway")
	cmd.Dir = proj
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
	}
	t.Cleanup(stop)
	return logPath, stop
}

func postGraphQL(t *testing.T, client *http.Client, port int, variables map[string]any) []byte {
	t.Helper()
	const query = "mutation($message:String!,$session:String!,$model:String){chat{send(message:$message,sessionKey:$session,model:$model){ok}}}"
	result, err := requestGraphQL(client, port, query, variables)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func requestGraphQL(client *http.Client, port int, query string, variables map[string]any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, err
	}
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/graphql", port), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	result, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || bytes.Contains(result, []byte(`"errors"`)) {
		return nil, fmt.Errorf("moltis GraphQL failed (%s): %s", resp.Status, result)
	}
	return result, nil
}

func moltisHistory(client *http.Client, port int, session string) ([]byte, error) {
	return requestGraphQL(client, port,
		"query($session:String!){chat{history(sessionKey:$session)}}",
		map[string]any{"session": session})
}

func moltisHistoryContains(client *http.Client, port int, session, role, substring string) bool {
	var response struct {
		Data struct {
			Chat struct {
				History []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"history"`
			} `json:"chat"`
		} `json:"data"`
	}
	history, err := moltisHistory(client, port, session)
	if err != nil {
		return false
	}
	if err := json.Unmarshal(history, &response); err != nil {
		return false
	}
	for _, message := range response.Data.Chat.History {
		if message.Role == role && strings.Contains(message.Content, substring) {
			return true
		}
	}
	return false
}

func waitMoltis(t *testing.T, timeout time.Duration, ready func() bool, what, logPath string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	logs, _ := os.ReadFile(logPath)
	t.Fatalf("timed out waiting for Moltis %s\nGateway logs:\n%s", what, tail(string(logs), 6000))
}

func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	return err == nil && bytes.Contains(data, []byte(needle))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasCanonicalTool(evs []event, canonical agenthooks.CanonicalTool) bool {
	for _, event := range typedToolPres(evs) {
		if event.Canonical == string(canonical) {
			return true
		}
	}
	return false
}

func requireStableMoltisToolID(t *testing.T, evs []event) {
	t.Helper()
	type toolPayload struct {
		turnID    string
		arguments string
	}
	preIDs := make(map[string]toolPayload)
	postIDs := make(map[string]toolPayload)
	for _, event := range evs {
		if event.Typed || (event.Native != "BeforeToolCall" && event.Native != "AfterToolCall") {
			continue
		}
		var payload struct {
			ToolCallID string `json:"tool_call_id"`
			TurnID     string `json:"turn_id"`
			Arguments  any    `json:"arguments"`
		}
		if err := json.Unmarshal(event.Raw, &payload); err != nil {
			t.Fatalf("decode Moltis tool hook payload: %v", err)
		}
		if payload.ToolCallID == "" {
			t.Fatalf("Moltis %s omitted tool_call_id: %s", event.Native, event.Raw)
		}
		if payload.TurnID == "" {
			t.Fatalf("Moltis %s omitted turn_id: %s", event.Native, event.Raw)
		}
		arguments, err := json.Marshal(payload.Arguments)
		if err != nil {
			t.Fatalf("encode Moltis %s arguments: %v", event.Native, err)
		}
		if payload.Arguments == nil {
			t.Fatalf("Moltis %s omitted arguments: %s", event.Native, event.Raw)
		}
		entry := toolPayload{turnID: payload.TurnID, arguments: string(arguments)}
		if event.Native == "BeforeToolCall" {
			preIDs[payload.ToolCallID] = entry
		} else {
			postIDs[payload.ToolCallID] = entry
		}
	}
	if len(preIDs) == 0 || len(postIDs) == 0 {
		t.Fatalf("missing Moltis tool lifecycle IDs; pre=%v post=%v", preIDs, postIDs)
	}
	for id, pre := range preIDs {
		post, ok := postIDs[id]
		if !ok {
			t.Fatalf("Moltis BeforeToolCall ID %q has no matching AfterToolCall; pre=%v post=%v", id, preIDs, postIDs)
		}
		if pre != post {
			t.Fatalf("Moltis tool lifecycle correlation changed for %q: pre=%+v post=%+v", id, pre, post)
		}
	}
	for id := range postIDs {
		if _, ok := preIDs[id]; !ok {
			t.Fatalf("Moltis AfterToolCall ID %q has no matching BeforeToolCall; pre=%v post=%v", id, preIDs, postIDs)
		}
	}
}

func requireMoltisTurnCorrelation(t *testing.T, evs []event) {
	t.Helper()
	turnIDs := make(map[string]string)
	for _, event := range evs {
		if event.Typed {
			continue
		}
		switch event.Native {
		case "MessageReceived", "BeforeToolCall", "AfterToolCall", "AgentEnd":
		default:
			continue
		}
		var payload struct {
			TurnID string `json:"turn_id"`
		}
		if err := json.Unmarshal(event.Raw, &payload); err != nil {
			t.Fatalf("decode Moltis %s turn id: %v", event.Native, err)
		}
		if payload.TurnID == "" {
			t.Fatalf("Moltis %s omitted turn_id: %s", event.Native, event.Raw)
		}
		turnIDs[event.Native] = payload.TurnID
	}
	want := turnIDs["MessageReceived"]
	if want == "" {
		t.Fatalf("missing MessageReceived turn id: %v", turnIDs)
	}
	for _, native := range []string{"BeforeToolCall", "AfterToolCall", "AgentEnd"} {
		if got := turnIDs[native]; got != want {
			t.Fatalf("Moltis %s turn_id = %q, want %q; all=%v", native, got, want, turnIDs)
		}
	}
}

func requireMoltisMessageLifecycle(t *testing.T, evs []event) {
	t.Helper()
	type lifecycleEvent struct {
		index      int
		native     string
		sessionKey string
		content    string
	}
	var lifecycle []lifecycleEvent
	for i, event := range evs {
		if event.Typed || (event.Native != "MessageSending" && event.Native != "MessageSent") {
			continue
		}
		var payload struct {
			SessionKey string `json:"session_key"`
			Content    string `json:"content"`
		}
		if err := json.Unmarshal(event.Raw, &payload); err != nil {
			t.Fatalf("decode Moltis %s payload: %v", event.Native, err)
		}
		if payload.SessionKey == "" || payload.Content == "" {
			t.Fatalf("Moltis %s payload lacks session/content correlation: %s", event.Native, event.Raw)
		}
		lifecycle = append(lifecycle, lifecycleEvent{
			index:      i,
			native:     event.Native,
			sessionKey: payload.SessionKey,
			content:    payload.Content,
		})
	}
	for _, sent := range lifecycle {
		if sent.native != "MessageSent" {
			continue
		}
		for _, sending := range lifecycle {
			if sending.native == "MessageSending" &&
				sending.index < sent.index &&
				sending.sessionKey == sent.sessionKey &&
				sending.content == sent.content {
				return
			}
		}
	}
	t.Fatalf("no Moltis MessageSending event had a later matching MessageSent event: %+v", lifecycle)
}
