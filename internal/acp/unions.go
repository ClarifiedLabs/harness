package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func (s MCPServer) MarshalJSON() ([]byte, error) {
	transport := s.Type
	if transport == "" {
		transport = MCPTransportStdio
	}
	switch transport {
	case MCPTransportStdio:
		args, env := s.Args, s.Env
		if args == nil {
			args = []string{}
		}
		if env == nil {
			env = []EnvVariable{}
		}
		return json.Marshal(struct {
			Name    string        `json:"name"`
			Command string        `json:"command"`
			Args    []string      `json:"args"`
			Env     []EnvVariable `json:"env"`
			Meta    Meta          `json:"_meta,omitempty"`
		}{s.Name, s.Command, args, env, s.Meta})
	case MCPTransportHTTP, MCPTransportSSE:
		headers := s.Headers
		if headers == nil {
			headers = []HTTPHeader{}
		}
		return json.Marshal(struct {
			Type    MCPTransport `json:"type"`
			Name    string       `json:"name"`
			URL     string       `json:"url"`
			Headers []HTTPHeader `json:"headers"`
			Meta    Meta         `json:"_meta,omitempty"`
		}{transport, s.Name, s.URL, headers, s.Meta})
	default:
		return json.Marshal(struct {
			Type MCPTransport `json:"type"`
			Name string       `json:"name"`
			Meta Meta         `json:"_meta,omitempty"`
		}{transport, s.Name, s.Meta})
	}
}

func (s *MCPServer) UnmarshalJSON(data []byte) error {
	type wire struct {
		Type    MCPTransport  `json:"type"`
		Name    string        `json:"name"`
		Command string        `json:"command"`
		Args    []string      `json:"args"`
		Env     []EnvVariable `json:"env"`
		URL     string        `json:"url"`
		Headers []HTTPHeader  `json:"headers"`
		Meta    Meta          `json:"_meta"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return fmt.Errorf("acp: decode MCP server: %w", err)
	}
	if w.Type == "" {
		w.Type = MCPTransportStdio
	}
	*s = MCPServer(w)
	return nil
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if !knownContentType(b.Type) && len(b.Raw) != 0 {
		return cloneRawObject(b.Raw, "content block")
	}
	if b.Type == "" {
		// Never emit a discriminator our own decoder would reject.
		return nil, fmt.Errorf("acp: content block: missing type")
	}
	var payload any
	switch b.Type {
	case ContentTypeText:
		payload = struct {
			Text        string       `json:"text"`
			Annotations *Annotations `json:"annotations,omitempty"`
			Meta        Meta         `json:"_meta,omitempty"`
		}{b.Text, b.Annotations, b.Meta}
	case ContentTypeImage:
		payload = struct {
			Data        string       `json:"data"`
			MIMEType    string       `json:"mimeType"`
			URI         string       `json:"uri,omitempty"`
			Annotations *Annotations `json:"annotations,omitempty"`
			Meta        Meta         `json:"_meta,omitempty"`
		}{b.Data, b.MIMEType, b.URI, b.Annotations, b.Meta}
	case ContentTypeAudio:
		payload = struct {
			Data        string       `json:"data"`
			MIMEType    string       `json:"mimeType"`
			Annotations *Annotations `json:"annotations,omitempty"`
			Meta        Meta         `json:"_meta,omitempty"`
		}{b.Data, b.MIMEType, b.Annotations, b.Meta}
	case ContentTypeResourceLink:
		payload = struct {
			URI         string       `json:"uri"`
			Name        string       `json:"name"`
			Title       string       `json:"title,omitempty"`
			Description string       `json:"description,omitempty"`
			MIMEType    string       `json:"mimeType,omitempty"`
			Size        *int64       `json:"size,omitempty"`
			Annotations *Annotations `json:"annotations,omitempty"`
			Meta        Meta         `json:"_meta,omitempty"`
		}{b.URI, b.Name, b.Title, b.Description, b.MIMEType, b.Size, b.Annotations, b.Meta}
	case ContentTypeResource:
		payload = struct {
			Resource    *EmbeddedResource `json:"resource"`
			Annotations *Annotations      `json:"annotations,omitempty"`
			Meta        Meta              `json:"_meta,omitempty"`
		}{b.Resource, b.Annotations, b.Meta}
	default:
		payload = struct {
			Meta Meta `json:"_meta,omitempty"`
		}{b.Meta}
	}
	return marshalTagged("type", string(b.Type), payload)
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var head struct {
		Type ContentType `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return fmt.Errorf("acp: decode content block discriminator: %w", err)
	}
	if head.Type == "" {
		return fmt.Errorf("acp: content block: missing type")
	}
	type wire ContentBlock
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("acp: decode content block: %w", err)
	}
	decoded.Type = head.Type
	if !knownContentType(head.Type) {
		decoded.Raw = bytes.Clone(data)
	}
	*b = ContentBlock(decoded)
	return nil
}

func knownContentType(t ContentType) bool {
	switch t {
	case ContentTypeText, ContentTypeImage, ContentTypeAudio, ContentTypeResourceLink, ContentTypeResource:
		return true
	default:
		return false
	}
}

func (u SessionUpdate) MarshalJSON() ([]byte, error) {
	if !knownUpdateKind(u.Kind) && len(u.Raw) != 0 {
		return cloneRawObject(u.Raw, "session update")
	}
	if u.Kind == "" {
		return nil, fmt.Errorf("acp: session update: missing sessionUpdate")
	}
	var payload any
	switch u.Kind {
	case UpdateUserMessageChunk, UpdateAgentMessageChunk, UpdateAgentThoughtChunk:
		payload = u.ContentChunk
	case UpdateToolCall:
		payload = u.ToolCall
	case UpdateToolCallUpdate:
		payload = u.ToolCallUpdate
	case UpdatePlan:
		payload = u.Plan
	case UpdateAvailableCommands:
		payload = u.AvailableCommands
	case UpdateCurrentMode:
		payload = u.CurrentMode
	case UpdateConfigOption:
		payload = u.ConfigOption
	case UpdateSessionInfo:
		payload = u.SessionInfo
	case UpdateUsage:
		payload = u.Usage
	default:
		payload = struct{}{}
	}
	if payload == nil {
		return nil, fmt.Errorf("acp: session update %q has no payload", u.Kind)
	}
	return marshalTagged("sessionUpdate", string(u.Kind), payload)
}

func (u *SessionUpdate) UnmarshalJSON(data []byte) error {
	var head struct {
		Kind UpdateKind `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return fmt.Errorf("acp: decode session update discriminator: %w", err)
	}
	if head.Kind == "" {
		return fmt.Errorf("acp: session update: missing sessionUpdate")
	}
	out := SessionUpdate{Kind: head.Kind}
	var target any
	switch head.Kind {
	case UpdateUserMessageChunk, UpdateAgentMessageChunk, UpdateAgentThoughtChunk:
		out.ContentChunk = new(ContentChunk)
		target = out.ContentChunk
	case UpdateToolCall:
		out.ToolCall = new(ToolCall)
		target = out.ToolCall
	case UpdateToolCallUpdate:
		out.ToolCallUpdate = new(ToolCallUpdate)
		target = out.ToolCallUpdate
	case UpdatePlan:
		out.Plan = new(Plan)
		target = out.Plan
	case UpdateAvailableCommands:
		out.AvailableCommands = new(AvailableCommandsUpdate)
		target = out.AvailableCommands
	case UpdateCurrentMode:
		out.CurrentMode = new(CurrentModeUpdate)
		target = out.CurrentMode
	case UpdateConfigOption:
		out.ConfigOption = new(ConfigOptionUpdate)
		target = out.ConfigOption
	case UpdateSessionInfo:
		out.SessionInfo = new(SessionInfoUpdate)
		target = out.SessionInfo
	case UpdateUsage:
		out.Usage = new(UsageUpdate)
		target = out.Usage
	default:
		out.Raw = bytes.Clone(data)
		*u = out
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("acp: decode session update %q: %w", head.Kind, err)
	}
	*u = out
	return nil
}

func knownUpdateKind(k UpdateKind) bool {
	switch k {
	case UpdateUserMessageChunk, UpdateAgentMessageChunk, UpdateAgentThoughtChunk,
		UpdateToolCall, UpdateToolCallUpdate, UpdatePlan, UpdateAvailableCommands,
		UpdateCurrentMode, UpdateConfigOption, UpdateSessionInfo, UpdateUsage:
		return true
	default:
		return false
	}
}

func (c ToolCallContent) MarshalJSON() ([]byte, error) {
	if !knownToolCallContentType(c.Type) && len(c.Raw) != 0 {
		return cloneRawObject(c.Raw, "tool call content")
	}
	if c.Type == "" {
		return nil, fmt.Errorf("acp: tool call content: missing type")
	}
	var payload any
	switch c.Type {
	case ToolCallContentBlock:
		payload = struct {
			Content *ContentBlock `json:"content"`
			Meta    Meta          `json:"_meta,omitempty"`
		}{c.Content, c.Meta}
	case ToolCallContentDiff:
		payload = struct {
			Path    string  `json:"path"`
			OldText *string `json:"oldText,omitempty"`
			NewText string  `json:"newText"`
			Meta    Meta    `json:"_meta,omitempty"`
		}{c.Path, c.OldText, c.NewText, c.Meta}
	case ToolCallContentTerminal:
		payload = struct {
			TerminalID string `json:"terminalId"`
			Meta       Meta   `json:"_meta,omitempty"`
		}{c.TerminalID, c.Meta}
	default:
		payload = struct{}{}
	}
	return marshalTagged("type", string(c.Type), payload)
}

func (c *ToolCallContent) UnmarshalJSON(data []byte) error {
	type wire ToolCallContent
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("acp: decode tool call content: %w", err)
	}
	if decoded.Type == "" {
		return fmt.Errorf("acp: tool call content: missing type")
	}
	if !knownToolCallContentType(decoded.Type) {
		decoded.Raw = bytes.Clone(data)
	}
	*c = ToolCallContent(decoded)
	return nil
}

func knownToolCallContentType(t ToolCallContentType) bool {
	return t == ToolCallContentBlock || t == ToolCallContentDiff || t == ToolCallContentTerminal
}

func marshalTagged(key, value string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("acp: %s %q has no payload", key, value)
	}
	tag, _ := json.Marshal(value)
	fields[key] = tag
	return json.Marshal(fields)
}

func cloneRawObject(raw json.RawMessage, what string) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, fmt.Errorf("acp: invalid raw %s", what)
	}
	return bytes.Clone(trimmed), nil
}

func (r NewSessionRequest) MarshalJSON() ([]byte, error) {
	type alias NewSessionRequest
	if r.MCPServers == nil {
		r.MCPServers = []MCPServer{}
	}
	return json.Marshal(alias(r))
}

func (r PromptRequest) MarshalJSON() ([]byte, error) {
	type alias PromptRequest
	if r.Prompt == nil {
		r.Prompt = []ContentBlock{}
	}
	return json.Marshal(alias(r))
}

func (r RequestPermissionRequest) MarshalJSON() ([]byte, error) {
	type alias RequestPermissionRequest
	if r.Options == nil {
		r.Options = []PermissionOption{}
	}
	return json.Marshal(alias(r))
}

func (p Plan) MarshalJSON() ([]byte, error) {
	type alias Plan
	if p.Entries == nil {
		p.Entries = []PlanEntry{}
	}
	return json.Marshal(alias(p))
}

func (u AvailableCommandsUpdate) MarshalJSON() ([]byte, error) {
	type alias AvailableCommandsUpdate
	if u.AvailableCommands == nil {
		u.AvailableCommands = []AvailableCommand{}
	}
	return json.Marshal(alias(u))
}

func (u ConfigOptionUpdate) MarshalJSON() ([]byte, error) {
	type alias ConfigOptionUpdate
	if u.ConfigOptions == nil {
		u.ConfigOptions = []json.RawMessage{}
	}
	return json.Marshal(alias(u))
}
