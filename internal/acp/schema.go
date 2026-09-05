package acp

import "encoding/json"

// Stable implementation limits. ACP itself does not prescribe these limits;
// they keep untrusted protocol values practical to retain or surface.
const (
	MaxIdentifierBytes       = 1024
	MaxNameBytes             = 4096
	MaxTextBytes             = 1 << 20
	MaxBinaryDataBytes       = 16 << 20
	MaxCollectionItems       = 1024
	MaxModelFacingTextBytes  = 64 << 10
	MaxModelFacingUpdateText = 256 << 10
)

// Meta is extension metadata reserved by ACP. Its keys and values are opaque.
type Meta = json.RawMessage

// Implementation identifies a client or agent implementation.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
	Meta    Meta   `json:"_meta,omitempty"`
}

// InitializeRequest is the client payload for initialize.
type InitializeRequest struct {
	ProtocolVersion    Version            `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities,omitempty"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
	Meta               Meta               `json:"_meta,omitempty"`
}

// InitializeResponse is the agent result for initialize.
type InitializeResponse struct {
	ProtocolVersion   Version           `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities,omitempty"`
	AuthMethods       []AuthMethod      `json:"authMethods,omitempty"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
	Meta              Meta              `json:"_meta,omitempty"`
}

// ClientCapabilities advertises optional methods implemented by a client.
type ClientCapabilities struct {
	FS          FileSystemCapabilities     `json:"fs,omitempty"`
	Terminal    bool                       `json:"terminal,omitempty"`
	Session     *ClientSessionCapabilities `json:"session,omitempty"`
	Auth        AuthCapabilities           `json:"auth,omitempty"`
	Elicitation json.RawMessage            `json:"elicitation,omitempty"`
	Meta        Meta                       `json:"_meta,omitempty"`
}

type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile,omitempty"`
	WriteTextFile bool `json:"writeTextFile,omitempty"`
	Meta          Meta `json:"_meta,omitempty"`
}

type AuthCapabilities struct {
	Terminal bool `json:"terminal,omitempty"`
	Meta     Meta `json:"_meta,omitempty"`
}

type ClientSessionCapabilities struct {
	ConfigOptions json.RawMessage `json:"configOptions,omitempty"`
	Meta          Meta            `json:"_meta,omitempty"`
}

// AgentCapabilities advertises optional methods and content support. The core
// session new, prompt, update, and cancel methods require no capability bit.
type AgentCapabilities struct {
	LoadSession         bool                  `json:"loadSession,omitempty"`
	PromptCapabilities  PromptCapabilities    `json:"promptCapabilities,omitempty"`
	MCPCapabilities     MCPCapabilities       `json:"mcpCapabilities,omitempty"`
	SessionCapabilities SessionCapabilities   `json:"sessionCapabilities,omitempty"`
	Auth                AgentAuthCapabilities `json:"auth,omitempty"`
	Meta                Meta                  `json:"_meta,omitempty"`
}

type PromptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
	Meta            Meta `json:"_meta,omitempty"`
}

type MCPCapabilities struct {
	HTTP bool `json:"http,omitempty"`
	SSE  bool `json:"sse,omitempty"`
	Meta Meta `json:"_meta,omitempty"`
}

// Capability is an empty capability marker. A non-nil pointer encodes as {} and
// advertises support; an omitted or null pointer does not.
type Capability struct {
	Meta Meta `json:"_meta,omitempty"`
}

type SessionCapabilities struct {
	List                  *Capability `json:"list,omitempty"`
	Delete                *Capability `json:"delete,omitempty"`
	AdditionalDirectories *Capability `json:"additionalDirectories,omitempty"`
	Resume                *Capability `json:"resume,omitempty"`
	Close                 *Capability `json:"close,omitempty"`
	Meta                  Meta        `json:"_meta,omitempty"`
}

type AgentAuthCapabilities struct {
	Logout *Capability `json:"logout,omitempty"`
	Meta   Meta        `json:"_meta,omitempty"`
}

// AuthMethod describes an agent-handled (empty Type) or terminal authentication
// method. Authentication calls themselves are outside this package's slice.
type AuthMethod struct {
	Type        string            `json:"type,omitempty"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Meta        Meta              `json:"_meta,omitempty"`
}

// SessionID and related IDs are opaque strings.
type SessionID string
type MessageID string
type ToolCallID string

// NewSessionRequest creates a conversation rooted at an absolute working
// directory. A stdio MCP server omits Type on the wire.
type NewSessionRequest struct {
	CWD                   string      `json:"cwd"`
	AdditionalDirectories []string    `json:"additionalDirectories,omitempty"`
	MCPServers            []MCPServer `json:"mcpServers"`
	Meta                  Meta        `json:"_meta,omitempty"`
}

type NewSessionResponse struct {
	SessionID     SessionID       `json:"sessionId"`
	Modes         json.RawMessage `json:"modes,omitempty"`
	ConfigOptions json.RawMessage `json:"configOptions,omitempty"`
	Meta          Meta            `json:"_meta,omitempty"`
}

type MCPTransport string

const (
	MCPTransportStdio MCPTransport = "stdio"
	MCPTransportHTTP  MCPTransport = "http"
	MCPTransportSSE   MCPTransport = "sse"
)

// MCPServer is the ACP union of stdio, HTTP, and SSE MCP definitions. Type is
// MCPTransportStdio after decoding a stdio definition but is omitted on encode.
type MCPServer struct {
	Type    MCPTransport  `json:"-"`
	Name    string        `json:"name"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Env     []EnvVariable `json:"env,omitempty"`
	URL     string        `json:"url,omitempty"`
	Headers []HTTPHeader  `json:"headers,omitempty"`
	Meta    Meta          `json:"_meta,omitempty"`
}

type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Meta  Meta   `json:"_meta,omitempty"`
}

type HTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Meta  Meta   `json:"_meta,omitempty"`
}

// PromptRequest starts one prompt turn.
type PromptRequest struct {
	SessionID SessionID      `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
	Meta      Meta           `json:"_meta,omitempty"`
}

type StopReason string

const (
	StopReasonEndTurn         StopReason = "end_turn"
	StopReasonMaxTokens       StopReason = "max_tokens"
	StopReasonMaxTurnRequests StopReason = "max_turn_requests"
	StopReasonRefusal         StopReason = "refusal"
	StopReasonCancelled       StopReason = "cancelled"
)

type PromptResponse struct {
	StopReason StopReason `json:"stopReason"`
	Meta       Meta       `json:"_meta,omitempty"`
}

type ContentType string

const (
	ContentTypeText         ContentType = "text"
	ContentTypeImage        ContentType = "image"
	ContentTypeAudio        ContentType = "audio"
	ContentTypeResourceLink ContentType = "resource_link"
	ContentTypeResource     ContentType = "resource"
)

// ContentBlock is ACP's MCP-compatible content union. Fields relevant to Type
// are emitted. Unknown variants are retained byte-for-byte in Raw.
type ContentBlock struct {
	Type        ContentType       `json:"type"`
	Text        string            `json:"text,omitempty"`
	Data        string            `json:"data,omitempty"`
	MIMEType    string            `json:"mimeType,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Name        string            `json:"name,omitempty"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Size        *int64            `json:"size,omitempty"`
	Resource    *EmbeddedResource `json:"resource,omitempty"`
	Annotations *Annotations      `json:"annotations,omitempty"`
	Meta        Meta              `json:"_meta,omitempty"`
	Raw         json.RawMessage   `json:"-"`
}

type EmbeddedResource struct {
	URI      string  `json:"uri"`
	MIMEType string  `json:"mimeType,omitempty"`
	Text     *string `json:"text,omitempty"`
	Blob     *string `json:"blob,omitempty"`
	Meta     Meta    `json:"_meta,omitempty"`
}

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Annotations struct {
	Audience     []Role   `json:"audience,omitempty"`
	LastModified string   `json:"lastModified,omitempty"`
	Priority     *float64 `json:"priority,omitempty"`
	Meta         Meta     `json:"_meta,omitempty"`
}

// SessionUpdateNotification carries an agent update for a session.
type SessionUpdateNotification struct {
	SessionID SessionID     `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
	Meta      Meta          `json:"_meta,omitempty"`
}

// SessionNotification is the schema's name for SessionUpdateNotification.
type SessionNotification = SessionUpdateNotification

type UpdateKind string

const (
	UpdateUserMessageChunk  UpdateKind = "user_message_chunk"
	UpdateAgentMessageChunk UpdateKind = "agent_message_chunk"
	UpdateAgentThoughtChunk UpdateKind = "agent_thought_chunk"
	UpdateToolCall          UpdateKind = "tool_call"
	UpdateToolCallUpdate    UpdateKind = "tool_call_update"
	UpdatePlan              UpdateKind = "plan"
	UpdateAvailableCommands UpdateKind = "available_commands_update"
	UpdateCurrentMode       UpdateKind = "current_mode_update"
	UpdateConfigOption      UpdateKind = "config_option_update"
	UpdateSessionInfo       UpdateKind = "session_info_update"
	UpdateUsage             UpdateKind = "usage_update"
)

// SessionUpdate is a discriminated union. Exactly one typed payload pointer is
// populated for a known Kind. Unknown kinds decode successfully into Raw.
type SessionUpdate struct {
	Kind              UpdateKind               `json:"-"`
	ContentChunk      *ContentChunk            `json:"-"`
	ToolCall          *ToolCall                `json:"-"`
	ToolCallUpdate    *ToolCallUpdate          `json:"-"`
	Plan              *Plan                    `json:"-"`
	AvailableCommands *AvailableCommandsUpdate `json:"-"`
	CurrentMode       *CurrentModeUpdate       `json:"-"`
	ConfigOption      *ConfigOptionUpdate      `json:"-"`
	SessionInfo       *SessionInfoUpdate       `json:"-"`
	Usage             *UsageUpdate             `json:"-"`
	Raw               json.RawMessage          `json:"-"`
}

func (u SessionUpdate) Unknown() bool { return len(u.Raw) != 0 }

type ContentChunk struct {
	Content   ContentBlock `json:"content"`
	MessageID MessageID    `json:"messageId,omitempty"`
	Meta      Meta         `json:"_meta,omitempty"`
}

type ToolKind string

const (
	ToolKindRead       ToolKind = "read"
	ToolKindEdit       ToolKind = "edit"
	ToolKindDelete     ToolKind = "delete"
	ToolKindMove       ToolKind = "move"
	ToolKindSearch     ToolKind = "search"
	ToolKindExecute    ToolKind = "execute"
	ToolKindThink      ToolKind = "think"
	ToolKindFetch      ToolKind = "fetch"
	ToolKindSwitchMode ToolKind = "switch_mode"
	ToolKindOther      ToolKind = "other"
)

type ToolCallStatus string

const (
	ToolCallPending    ToolCallStatus = "pending"
	ToolCallInProgress ToolCallStatus = "in_progress"
	ToolCallCompleted  ToolCallStatus = "completed"
	ToolCallFailed     ToolCallStatus = "failed"
)

type ToolCall struct {
	ToolCallID ToolCallID         `json:"toolCallId"`
	Title      string             `json:"title"`
	Kind       ToolKind           `json:"kind,omitempty"`
	Status     ToolCallStatus     `json:"status,omitempty"`
	Content    []ToolCallContent  `json:"content,omitempty"`
	Locations  []ToolCallLocation `json:"locations,omitempty"`
	RawInput   json.RawMessage    `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage    `json:"rawOutput,omitempty"`
	Meta       Meta               `json:"_meta,omitempty"`
}

type ToolCallUpdate struct {
	ToolCallID ToolCallID          `json:"toolCallId"`
	Kind       *ToolKind           `json:"kind,omitempty"`
	Status     *ToolCallStatus     `json:"status,omitempty"`
	Title      *string             `json:"title,omitempty"`
	Content    *[]ToolCallContent  `json:"content,omitempty"`
	Locations  *[]ToolCallLocation `json:"locations,omitempty"`
	RawInput   json.RawMessage     `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage     `json:"rawOutput,omitempty"`
	Meta       Meta                `json:"_meta,omitempty"`
}

type ToolCallLocation struct {
	Path string  `json:"path"`
	Line *uint32 `json:"line,omitempty"`
	Meta Meta    `json:"_meta,omitempty"`
}

type ToolCallContentType string

const (
	ToolCallContentBlock    ToolCallContentType = "content"
	ToolCallContentDiff     ToolCallContentType = "diff"
	ToolCallContentTerminal ToolCallContentType = "terminal"
)

// ToolCallContent is the content | diff | terminal union.
type ToolCallContent struct {
	Type       ToolCallContentType `json:"type"`
	Content    *ContentBlock       `json:"content,omitempty"`
	Path       string              `json:"path,omitempty"`
	OldText    *string             `json:"oldText,omitempty"`
	NewText    string              `json:"newText,omitempty"`
	TerminalID string              `json:"terminalId,omitempty"`
	Meta       Meta                `json:"_meta,omitempty"`
	Raw        json.RawMessage     `json:"-"`
}

type Plan struct {
	Entries []PlanEntry `json:"entries"`
	Meta    Meta        `json:"_meta,omitempty"`
}

type PlanEntryPriority string

const (
	PlanPriorityHigh   PlanEntryPriority = "high"
	PlanPriorityMedium PlanEntryPriority = "medium"
	PlanPriorityLow    PlanEntryPriority = "low"
)

type PlanEntryStatus string

const (
	PlanEntryPending    PlanEntryStatus = "pending"
	PlanEntryInProgress PlanEntryStatus = "in_progress"
	PlanEntryCompleted  PlanEntryStatus = "completed"
)

type PlanEntry struct {
	Content  string            `json:"content"`
	Priority PlanEntryPriority `json:"priority"`
	Status   PlanEntryStatus   `json:"status"`
	Meta     Meta              `json:"_meta,omitempty"`
}

type AvailableCommandsUpdate struct {
	AvailableCommands []AvailableCommand `json:"availableCommands"`
	Meta              Meta               `json:"_meta,omitempty"`
}

type AvailableCommand struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input,omitempty"`
	Meta        Meta            `json:"_meta,omitempty"`
}

type CurrentModeUpdate struct {
	CurrentModeID string `json:"currentModeId"`
	Meta          Meta   `json:"_meta,omitempty"`
}

type ConfigOptionUpdate struct {
	ConfigOptions []json.RawMessage `json:"configOptions"`
	Meta          Meta              `json:"_meta,omitempty"`
}

type SessionInfoUpdate struct {
	Title     *string `json:"title,omitempty"`
	UpdatedAt *string `json:"updatedAt,omitempty"`
	Meta      Meta    `json:"_meta,omitempty"`
}

type UsageUpdate struct {
	Used uint64 `json:"used"`
	Size uint64 `json:"size"`
	Cost *Cost  `json:"cost,omitempty"`
	Meta Meta   `json:"_meta,omitempty"`
}

type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Meta     Meta    `json:"_meta,omitempty"`
}

// CancelNotification cancels an active prompt turn.
type CancelNotification struct {
	SessionID SessionID `json:"sessionId"`
	Meta      Meta      `json:"_meta,omitempty"`
}

type CloseSessionRequest struct {
	SessionID SessionID `json:"sessionId"`
	Meta      Meta      `json:"_meta,omitempty"`
}

type CloseSessionResponse struct {
	Meta Meta `json:"_meta,omitempty"`
}

type PermissionOptionKind string

const (
	PermissionAllowOnce    PermissionOptionKind = "allow_once"
	PermissionAllowAlways  PermissionOptionKind = "allow_always"
	PermissionRejectOnce   PermissionOptionKind = "reject_once"
	PermissionRejectAlways PermissionOptionKind = "reject_always"
)

type PermissionOption struct {
	OptionID string               `json:"optionId"`
	Name     string               `json:"name"`
	Kind     PermissionOptionKind `json:"kind"`
	Meta     Meta                 `json:"_meta,omitempty"`
}

type RequestPermissionRequest struct {
	SessionID SessionID          `json:"sessionId"`
	ToolCall  ToolCallUpdate     `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
	Meta      Meta               `json:"_meta,omitempty"`
}

type PermissionOutcomeKind string

const (
	PermissionOutcomeCancelled PermissionOutcomeKind = "cancelled"
	PermissionOutcomeSelected  PermissionOutcomeKind = "selected"
)

type RequestPermissionOutcome struct {
	Outcome  PermissionOutcomeKind `json:"outcome"`
	OptionID string                `json:"optionId,omitempty"`
	Meta     Meta                  `json:"_meta,omitempty"`
}

type RequestPermissionResponse struct {
	Outcome RequestPermissionOutcome `json:"outcome"`
	Meta    Meta                     `json:"_meta,omitempty"`
}
