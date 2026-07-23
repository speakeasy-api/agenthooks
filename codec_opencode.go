package agenthooks

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// OpenCode has no spawned-process hook protocol; the shim plugin the install
// package generates (.opencode/plugin/agenthooks.ts) proxies in-process
// plugin hooks to this binary over NDJSON on stdio (§8): request {seq, hook, input, output}
// -> response {seq, output, error?}. The shim Object.assigns the returned
// output (arrays replaced wholesale) and re-throws error to keep
// block-the-tool behavior.

type opencodeFrame struct {
	Seq    int64           `json:"seq"`
	Hook   string          `json:"hook"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
}

type opencodeReply struct {
	Seq    int64          `json:"seq"`
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}

func opencodeKind(hook string) EventKind {
	switch hook {
	case "tool.execute.before":
		return KindToolPre
	case "tool.execute.after":
		return KindToolPost
	case "chat.message":
		return KindPromptSubmitted
	case "session.idle":
		return KindStop
	case "session.created":
		return KindSessionStart
	case "server.instance.disposed":
		return KindSessionEnd
	case "experimental.session.compacting":
		return KindCompactPre
	case "session.compacted":
		return KindCompactPost
	case "permission.asked":
		return KindPermission
	case "file.edited":
		return KindFileEdited
	case "chat.params", "chat.headers":
		return KindModelRequest
	case "experimental.text.complete":
		return KindModelResponse
	case "tui.toast.show":
		return KindNotification
	}
	return KindOther
}

// decodeOpenCodeLine decodes one NDJSON frame into a typed event. Raw is the
// verbatim frame, so both the hook input and the mutable output object stay
// reachable.
func decodeOpenCodeLine(v Variant, conf DetectionConfidence, now time.Time, line []byte) (any, error) {
	var fr opencodeFrame
	if err := json.Unmarshal(line, &fr); err != nil {
		return nil, err
	}
	return decodeOpenCodeFrame(v, conf, now, &fr, line)
}

func decodeOpenCodeFrame(v Variant, conf DetectionConfidence, now time.Time, fr *opencodeFrame, raw []byte) (any, error) {
	if fr.Hook == "" {
		return nil, errors.New("agenthooks: opencode frame missing hook name")
	}
	if fr.Hook == "message.part.updated" {
		if ev := decodeOpenCodeToolError(v, conf, now, fr, raw); ev != nil {
			return ev, nil
		}
	}
	var in struct {
		SessionID string `json:"sessionID"`
		CallID    string `json:"callID"`
		Tool      string `json:"tool"`
		Directory string `json:"directory"`
		Worktree  string `json:"worktree"`
		File      string `json:"file"`
		Title     string `json:"title"`
		Message   string `json:"message"`
		ParentID  string `json:"parentID"`
		// FinalMessage is spliced into the session.idle input by the shim,
		// which reads the transcript over the OpenCode SDK: no native hook or
		// bus event carries the completed assistant text.
		FinalMessage string `json:"finalMessage"`
	}
	_ = json.Unmarshal(fr.Input, &in) // input shape varies per hook; best-effort probe
	if in.SessionID == "" {
		if sid := rawField(fr.Output, "message.sessionID"); len(sid) > 0 {
			_ = json.Unmarshal(sid, &in.SessionID)
		}
	}
	cwd := in.Directory
	if cwd == "" {
		cwd = in.Worktree
	}
	kind := opencodeKind(fr.Hook)
	// A child session maps to the subagent lifecycle (§3.4).
	if in.ParentID != "" {
		switch kind {
		case KindSessionStart:
			kind = KindSubagentStart
		case KindStop:
			kind = KindSubagentStop
		}
	}
	base := Event{
		Provider:            ProviderOpenCode,
		Variant:             v,
		NativeName:          fr.Hook,
		Kind:                kind,
		Time:                now,
		DetectionConfidence: conf,
		Session: SessionInfo{
			ID:             in.SessionID,
			CWD:            cwd,
			WorkspaceRoots: rootsFor(cwd),
		},
		Raw: json.RawMessage(raw),
	}
	if in.ParentID != "" {
		base.Agent = &AgentInfo{ID: in.SessionID}
	}

	switch kind {
	case KindToolPre:
		args := rawField(fr.Output, "args")
		return &ToolPreEvent{Event: base, Tool: makeToolCall(base.Session, in.Tool, in.CallID, args, args)}, nil
	case KindToolPost, KindToolError:
		errMsg := ""
		if e := rawField(fr.Output, "error"); len(e) > 0 && string(e) != "null" {
			var s string
			if json.Unmarshal(e, &s) == nil {
				errMsg = s
			} else {
				errMsg = string(e)
			}
		}
		return &ToolPostEvent{
			Event:  base,
			Tool:   makeToolCall(base.Session, in.Tool, in.CallID, rawField(fr.Output, "args"), nil),
			Output: fr.Output,
			Failed: errMsg != "",
			Error:  errMsg,
		}, nil
	case KindPromptSubmitted:
		return &PromptEvent{Event: base, Prompt: opencodePromptText(fr.Output)}, nil
	case KindStop, KindSubagentStop:
		return &StopEvent{Event: base, FinalMessage: in.FinalMessage}, nil
	case KindSubagentStart:
		return &SubagentStartEvent{Event: base}, nil
	case KindPermission:
		return &PermissionEvent{Event: base, Tool: makeToolCall(base.Session, in.Tool, in.CallID, nil, nil)}, nil
	case KindSessionStart:
		return &SessionStartEvent{Event: base}, nil
	case KindSessionEnd:
		return &SessionEndEvent{Event: base}, nil
	case KindCompactPre, KindCompactPost:
		return &CompactEvent{Event: base}, nil
	case KindNotification:
		return &NotificationEvent{Event: base, Message: in.Message}, nil
	case KindFileEdited:
		return &FileEditedEvent{Event: base, Path: in.File}, nil
	case KindModelRequest, KindModelResponse:
		return &ModelEvent{Event: base}, nil
	}
	ev := base
	return &ev, nil
}

// decodeOpenCodeToolError lifts a failed tool call out of a
// message.part.updated bus frame: tool.execute.after does not fire when a
// tool errors, so the part's error state is the only failure signal. Frames
// carrying any other part shape decode to nil.
func decodeOpenCodeToolError(v Variant, conf DetectionConfidence, now time.Time, fr *opencodeFrame, raw []byte) *ToolPostEvent {
	var in struct {
		Part struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			CallID    string `json:"callID"`
			Tool      string `json:"tool"`
			State     struct {
				Status string          `json:"status"`
				Error  string          `json:"error"`
				Input  json.RawMessage `json:"input"`
			} `json:"state"`
		} `json:"part"`
	}
	if err := json.Unmarshal(fr.Input, &in); err != nil {
		return nil
	}
	if in.Part.Type != "tool" || in.Part.State.Status != "error" {
		return nil
	}
	base := Event{
		Provider:            ProviderOpenCode,
		Variant:             v,
		NativeName:          fr.Hook,
		Kind:                KindToolError,
		Time:                now,
		DetectionConfidence: conf,
		Session:             SessionInfo{ID: in.Part.SessionID},
		Raw:                 json.RawMessage(raw),
	}
	return &ToolPostEvent{
		Event:  base,
		Tool:   makeToolCall(base.Session, in.Part.Tool, in.Part.CallID, in.Part.State.Input, nil),
		Output: nil,
		Failed: true,
		Error:  in.Part.State.Error,
	}
}

// opencodePromptText joins the text parts of a chat.message output.
func opencodePromptText(output json.RawMessage) string {
	parts := rawField(output, "parts")
	if len(parts) == 0 {
		return ""
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(parts, &arr); err != nil {
		return ""
	}
	var texts []string
	for _, p := range arr {
		if p.Type == "text" && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// encodeOpenCodeReply builds the shim response frame (seq is filled by the
// serve loop). Deny intents become the re-thrown error; input rewrites and
// output replacements ride the merged output object.
func encodeOpenCodeReply(_ any, base *Event, d decisionCore) (*opencodeReply, error) {
	reply := &opencodeReply{}
	switch d.kind {
	case decDeny, decBlockPrompt:
		reply.Error = d.reason
		if reply.Error == "" {
			reply.Error = "blocked by agenthooks handler"
		}
	case decAsk:
		// Degraded before encode; reaching here is a policy-layer bug.
		return nil, ErrUnsupportedDecision
	}

	set := func(k string, v any) {
		if reply.Output == nil {
			reply.Output = map[string]any{}
		}
		reply.Output[k] = v
	}

	switch base.Kind {
	case KindToolPre:
		if d.hasUpdatedInput {
			set("args", d.updatedInput)
		}
	case KindToolPost, KindToolError:
		if d.hasReplacedOutput {
			set("output", d.replacedOutput)
		}
	case KindPromptSubmitted:
		if ctx := joinContext(d.context); ctx != "" {
			// Arrays are replaced wholesale by the shim, so append the
			// context to the original parts rather than returning a fragment.
			var parts []any
			if raw := rawField(base.Raw, "output.parts"); len(raw) > 0 {
				_ = json.Unmarshal(raw, &parts)
			}
			parts = append(parts, map[string]any{"type": "text", "text": ctx, "synthetic": true})
			set("parts", parts)
		}
	}
	return reply, nil
}
