// Package moltis provides typed views over native Moltis HookPayload JSON.
// Moltis shell hooks are process-per-event: the complete internally tagged
// payload arrives on stdin and Event.Raw retains it byte-for-byte.
package moltis

import (
	"encoding/json"

	"github.com/speakeasy-api/agenthooks"
	"github.com/speakeasy-api/agenthooks/internal/jsonx"
)

// ChannelBinding is Moltis' best-effort channel/session provenance block.
type ChannelBinding struct {
	Surface     string                     `json:"surface"`
	SessionKind string                     `json:"session_kind"`
	ChannelType string                     `json:"channel_type"`
	AccountID   string                     `json:"account_id"`
	ChatID      string                     `json:"chat_id"`
	OutboundTo  string                     `json:"outbound_to"`
	ChatType    string                     `json:"chat_type"`
	SenderID    string                     `json:"sender_id"`
	Extra       map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON preserves fields added to Moltis' nested channel provenance
// block just as view() preserves unknown top-level payload fields.
func (c *ChannelBinding) UnmarshalJSON(data []byte) error {
	type rawChannelBinding ChannelBinding
	var decoded rawChannelBinding
	if err := jsonx.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*c = ChannelBinding(decoded)
	return nil
}

type BeforeAgentStartInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	Model      string                     `json:"model"`
	Extra      map[string]json.RawMessage `json:"-"`
}

type AgentEndInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	TurnID     string                     `json:"turn_id"`
	Text       string                     `json:"text"`
	Iterations int                        `json:"iterations"`
	ToolCalls  int                        `json:"tool_calls"`
	Extra      map[string]json.RawMessage `json:"-"`
}

type BeforeLLMCallInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	Provider   string                     `json:"provider"`
	Model      string                     `json:"model"`
	Messages   json.RawMessage            `json:"messages"`
	ToolCount  int                        `json:"tool_count"`
	Iteration  int                        `json:"iteration"`
	Extra      map[string]json.RawMessage `json:"-"`
}

type AfterLLMCallInput struct {
	Event        string                     `json:"event"`
	SessionKey   string                     `json:"session_key"`
	Provider     string                     `json:"provider"`
	Model        string                     `json:"model"`
	Text         *string                    `json:"text"`
	ToolCalls    []json.RawMessage          `json:"tool_calls"`
	InputTokens  uint32                     `json:"input_tokens"`
	OutputTokens uint32                     `json:"output_tokens"`
	Iteration    int                        `json:"iteration"`
	Extra        map[string]json.RawMessage `json:"-"`
}

type BeforeCompactionInput struct {
	Event        string                     `json:"event"`
	SessionKey   string                     `json:"session_key"`
	MessageCount int                        `json:"message_count"`
	Extra        map[string]json.RawMessage `json:"-"`
}

type AfterCompactionInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	SummaryLen int                        `json:"summary_len"`
	Extra      map[string]json.RawMessage `json:"-"`
}

type MessageReceivedInput struct {
	Event          string                     `json:"event"`
	SessionKey     string                     `json:"session_key"`
	TurnID         string                     `json:"turn_id"`
	Content        string                     `json:"content"`
	Channel        *string                    `json:"channel"`
	ChannelBinding *ChannelBinding            `json:"channel_binding"`
	Extra          map[string]json.RawMessage `json:"-"`
}

type MessageInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	Content    string                     `json:"content"`
	Extra      map[string]json.RawMessage `json:"-"`
}

type BeforeToolCallInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	TurnID     string                     `json:"turn_id"`
	ToolCallID string                     `json:"tool_call_id"`
	ToolName   string                     `json:"tool_name"`
	Arguments  json.RawMessage            `json:"arguments"`
	Channel    *ChannelBinding            `json:"channel"`
	Extra      map[string]json.RawMessage `json:"-"`
}

type AfterToolCallInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	TurnID     string                     `json:"turn_id"`
	ToolCallID string                     `json:"tool_call_id"`
	ToolName   string                     `json:"tool_name"`
	Arguments  json.RawMessage            `json:"arguments"`
	Success    bool                       `json:"success"`
	Result     json.RawMessage            `json:"result"`
	Channel    *ChannelBinding            `json:"channel"`
	Extra      map[string]json.RawMessage `json:"-"`
}

type ToolResultPersistInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	ToolCallID string                     `json:"tool_call_id"`
	ToolName   string                     `json:"tool_name"`
	Result     json.RawMessage            `json:"result"`
	Channel    *ChannelBinding            `json:"channel"`
	Extra      map[string]json.RawMessage `json:"-"`
}

type SessionStartInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	Channel    *ChannelBinding            `json:"channel"`
	Extra      map[string]json.RawMessage `json:"-"`
}

type SessionEndInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	Extra      map[string]json.RawMessage `json:"-"`
}

type GatewayStartInput struct {
	Event   string                     `json:"event"`
	Address string                     `json:"address"`
	Extra   map[string]json.RawMessage `json:"-"`
}

type GatewayStopInput struct {
	Event string                     `json:"event"`
	Extra map[string]json.RawMessage `json:"-"`
}

type CommandInput struct {
	Event      string                     `json:"event"`
	SessionKey string                     `json:"session_key"`
	Action     string                     `json:"action"`
	SenderID   *string                    `json:"sender_id"`
	Extra      map[string]json.RawMessage `json:"-"`
}

func view[T any](e *agenthooks.Event, native string) (*T, bool) {
	if e == nil || e.Provider != agenthooks.ProviderMoltis || e.NativeName != native {
		return nil, false
	}
	var value T
	if err := jsonx.Unmarshal(e.Raw, &value); err != nil {
		return nil, false
	}
	return &value, true
}

func BeforeAgentStart(e *agenthooks.Event) (*BeforeAgentStartInput, bool) {
	return view[BeforeAgentStartInput](e, "BeforeAgentStart")
}
func AgentEnd(e *agenthooks.Event) (*AgentEndInput, bool) {
	return view[AgentEndInput](e, "AgentEnd")
}
func BeforeLLMCall(e *agenthooks.Event) (*BeforeLLMCallInput, bool) {
	return view[BeforeLLMCallInput](e, "BeforeLLMCall")
}
func AfterLLMCall(e *agenthooks.Event) (*AfterLLMCallInput, bool) {
	return view[AfterLLMCallInput](e, "AfterLLMCall")
}
func BeforeCompaction(e *agenthooks.Event) (*BeforeCompactionInput, bool) {
	return view[BeforeCompactionInput](e, "BeforeCompaction")
}
func AfterCompaction(e *agenthooks.Event) (*AfterCompactionInput, bool) {
	return view[AfterCompactionInput](e, "AfterCompaction")
}
func MessageReceived(e *agenthooks.Event) (*MessageReceivedInput, bool) {
	return view[MessageReceivedInput](e, "MessageReceived")
}
func MessageSending(e *agenthooks.Event) (*MessageInput, bool) {
	return view[MessageInput](e, "MessageSending")
}
func MessageSent(e *agenthooks.Event) (*MessageInput, bool) {
	return view[MessageInput](e, "MessageSent")
}
func BeforeToolCall(e *agenthooks.Event) (*BeforeToolCallInput, bool) {
	return view[BeforeToolCallInput](e, "BeforeToolCall")
}
func AfterToolCall(e *agenthooks.Event) (*AfterToolCallInput, bool) {
	return view[AfterToolCallInput](e, "AfterToolCall")
}
func ToolResultPersist(e *agenthooks.Event) (*ToolResultPersistInput, bool) {
	return view[ToolResultPersistInput](e, "ToolResultPersist")
}
func SessionStart(e *agenthooks.Event) (*SessionStartInput, bool) {
	return view[SessionStartInput](e, "SessionStart")
}
func SessionEnd(e *agenthooks.Event) (*SessionEndInput, bool) {
	return view[SessionEndInput](e, "SessionEnd")
}
func GatewayStart(e *agenthooks.Event) (*GatewayStartInput, bool) {
	return view[GatewayStartInput](e, "GatewayStart")
}
func GatewayStop(e *agenthooks.Event) (*GatewayStopInput, bool) {
	return view[GatewayStopInput](e, "GatewayStop")
}
func Command(e *agenthooks.Event) (*CommandInput, bool) {
	return view[CommandInput](e, "Command")
}
