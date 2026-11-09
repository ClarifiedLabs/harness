package mcp

import (
	"bytes"
	"encoding/json"
)

// Method and notification names used on the wire. Typed constants avoid
// stringly-typed routing drift across client and server.
const (
	MethodInitialize    = "initialize"
	MethodPing          = "ping"
	MethodListTools     = "tools/list"
	MethodCallTool      = "tools/call"
	MethodListResources = "resources/list"
	MethodReadResource  = "resources/read"

	NotifInitialized      = "notifications/initialized"
	NotifToolsListChanged = "notifications/tools/list_changed"
	NotifCancelled        = "notifications/cancelled"
)

// Implementation identifies a client or server peer. Only Name and Version are
// required by the spec; Title is an optional display name.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// ClientCapabilities is what a client advertises. A tools-only client sends an
// empty object; Experimental is kept raw so future additions round-trip without
// a schema change.
type ClientCapabilities struct {
	Experimental json.RawMessage `json:"experimental,omitempty"`
	Extensions   json.RawMessage `json:"extensions,omitempty"`
}

// ServerCapabilities is what a server advertises. Only Tools is interpreted;
// everything else is retained raw so it survives a round-trip but is otherwise
// ignored by this tools-only slice.
type ServerCapabilities struct {
	Tools     *ToolsCapability `json:"tools,omitempty"`
	Resources json.RawMessage  `json:"resources,omitempty"`
	Prompts   json.RawMessage  `json:"prompts,omitempty"`
	Logging   json.RawMessage  `json:"logging,omitempty"`
}

// ToolsCapability describes the server's tools support. ListChanged true means
// the server may emit notifications/tools/list_changed.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// InitializeParams is the client's initialize request payload.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the server's initialize response payload.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// Tool is a single tool definition. The JSON Schema fields and annotations are
// passthrough raw because this slice never interprets them; it only routes on
// Name and surfaces Title/Description.
type Tool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Meta         json.RawMessage `json:"_meta,omitempty"`
}

// MarshalJSON guarantees the required inputSchema object is never null.
func (t Tool) MarshalJSON() ([]byte, error) {
	type alias Tool
	if len(t.InputSchema) == 0 || bytes.Equal(bytes.TrimSpace(t.InputSchema), []byte("null")) {
		t.InputSchema = defaultInputSchema()
	}
	return json.Marshal(alias(t))
}

// ListToolsParams is the tools/list request payload. Cursor is omitted on the
// first page.
type ListToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ListToolsResult is one page of tools/list. A non-empty NextCursor means more
// pages remain.
type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// MarshalJSON guarantees the required tools array is never null.
func (r ListToolsResult) MarshalJSON() ([]byte, error) {
	type alias ListToolsResult
	if r.Tools == nil {
		r.Tools = []Tool{}
	}
	return json.Marshal(alias(r))
}

// Resource describes an MCP resource. UI-specific metadata remains raw.
type Resource struct {
	URI         string          `json:"uri"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Size        *int64          `json:"size,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// ListResourcesParams is the resources/list request payload.
type ListResourcesParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ListResourcesResult is one page of resources/list.
type ListResourcesResult struct {
	Resources  []Resource      `json:"resources"`
	NextCursor string          `json:"nextCursor,omitempty"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

// MarshalJSON guarantees the required resources array is never null.
func (r ListResourcesResult) MarshalJSON() ([]byte, error) {
	type alias ListResourcesResult
	if r.Resources == nil {
		r.Resources = []Resource{}
	}
	return json.Marshal(alias(r))
}

// ResourceContents is a text or blob resource payload. Meta is passthrough raw
// so MCP Apps metadata survives a proxy round-trip.
type ResourceContents struct {
	URI      string          `json:"uri"`
	MimeType string          `json:"mimeType,omitempty"`
	Text     string          `json:"text,omitempty"`
	Blob     string          `json:"blob,omitempty"`
	Meta     json.RawMessage `json:"_meta,omitempty"`
}

// ReadResourceParams is the resources/read request payload. SourceTool is the
// optional MCP Apps routing hint at _meta.sourceTool; the original metadata is
// retained in Meta as well.
type ReadResourceParams struct {
	URI        string          `json:"uri"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
	SourceTool string          `json:"-"`
}

// UnmarshalJSON decodes the optional MCP Apps sourceTool hint while tolerating
// unrelated metadata keys.
func (p *ReadResourceParams) UnmarshalJSON(data []byte) error {
	type alias ReadResourceParams
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = ReadResourceParams(decoded)
	if len(p.Meta) == 0 || bytes.Equal(bytes.TrimSpace(p.Meta), []byte("null")) {
		return nil
	}
	var meta struct {
		SourceTool string `json:"sourceTool"`
	}
	if err := json.Unmarshal(p.Meta, &meta); err != nil {
		return err
	}
	p.SourceTool = meta.SourceTool
	return nil
}

// ReadResourceResult is the resources/read response payload.
type ReadResourceResult struct {
	Contents []ResourceContents `json:"contents"`
	Meta     json.RawMessage    `json:"_meta,omitempty"`
}

// MarshalJSON guarantees the required contents array is never null.
func (r ReadResourceResult) MarshalJSON() ([]byte, error) {
	type alias ReadResourceResult
	if r.Contents == nil {
		r.Contents = []ResourceContents{}
	}
	return json.Marshal(alias(r))
}

// CallToolParams is the tools/call request payload. Arguments is passthrough raw
// JSON validated against the tool's input schema by the server, not here.
type CallToolParams struct {
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
	SourceTool string          `json:"-"`
}

// UnmarshalJSON decodes the optional MCP Apps sourceTool routing hint.
func (p *CallToolParams) UnmarshalJSON(data []byte) error {
	var decoded struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
		Meta      json.RawMessage `json:"_meta,omitempty"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = CallToolParams{Name: decoded.Name, Arguments: decoded.Arguments, Meta: decoded.Meta}
	if len(p.Meta) == 0 || bytes.Equal(bytes.TrimSpace(p.Meta), []byte("null")) {
		return nil
	}
	var meta struct {
		SourceTool string `json:"sourceTool"`
	}
	if err := json.Unmarshal(p.Meta, &meta); err != nil {
		return err
	}
	p.SourceTool = meta.SourceTool
	return nil
}

// CallToolResult is the tools/call response payload. A tool-execution failure
// arrives here with IsError true on an otherwise successful JSON-RPC response;
// only protocol failures (unknown tool, bad params) are JSON-RPC errors.
type CallToolResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	Meta              json.RawMessage `json:"_meta,omitempty"`
}

// MarshalJSON guarantees the required content array is never null.
func (r CallToolResult) MarshalJSON() ([]byte, error) {
	type alias CallToolResult
	if r.Content == nil {
		r.Content = []ContentBlock{}
	}
	return json.Marshal(alias(r))
}

func defaultInputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

// ContentBlock is a single flat struct covering every content type
// (text | image | audio | resource_link | resource | …). A plain unmarshal
// populates whichever fields are present for the block's Type and leaves the
// rest zero; an unknown Type degrades to a Type-only block, which is the
// tolerant behavior this slice wants. No custom UnmarshalJSON is needed.
type ContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image / audio
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`

	// resource_link
	URI  string `json:"uri,omitempty"`
	Name string `json:"name,omitempty"`

	// embedded resource: kept raw, never interpreted.
	Resource json.RawMessage `json:"resource,omitempty"`

	// passthrough, ignored.
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// CancelledParams is the notifications/cancelled payload. RequestID is the raw
// JSON id of the request being cancelled, kept raw so it matches the original
// id's string-vs-int form exactly.
type CancelledParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}
