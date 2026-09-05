package acp

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strings"
)

func (r InitializeRequest) Validate() error {
	if _, err := NegotiateVersion(r.ProtocolVersion); err != nil {
		return err
	}
	if r.ClientInfo != nil {
		return r.ClientInfo.validate("clientInfo")
	}
	return nil
}

func (r InitializeResponse) Validate() error {
	if _, err := NegotiateVersion(r.ProtocolVersion); err != nil {
		return err
	}
	if r.AgentInfo != nil {
		if err := r.AgentInfo.validate("agentInfo"); err != nil {
			return err
		}
	}
	if len(r.AuthMethods) > MaxCollectionItems {
		return tooMany("authMethods", len(r.AuthMethods))
	}
	for i, method := range r.AuthMethods {
		if strings.TrimSpace(method.ID) == "" {
			return required(fmt.Sprintf("authMethods[%d].id", i))
		}
		if strings.TrimSpace(method.Name) == "" {
			return required(fmt.Sprintf("authMethods[%d].name", i))
		}
		if method.Type != "" && method.Type != "terminal" {
			return fmt.Errorf("acp: authMethods[%d].type: unsupported %q", i, method.Type)
		}
	}
	return nil
}

func (i Implementation) validate(field string) error {
	if strings.TrimSpace(i.Name) == "" {
		return required(field + ".name")
	}
	if strings.TrimSpace(i.Version) == "" {
		return required(field + ".version")
	}
	if err := bounded(field+".name", i.Name, MaxNameBytes); err != nil {
		return err
	}
	if err := bounded(field+".title", i.Title, MaxNameBytes); err != nil {
		return err
	}
	return bounded(field+".version", i.Version, MaxIdentifierBytes)
}

func (r NewSessionRequest) Validate() error {
	if strings.TrimSpace(r.CWD) == "" {
		return required("cwd")
	}
	if err := bounded("cwd", r.CWD, MaxTextBytes); err != nil {
		return err
	}
	if err := noNUL("cwd", r.CWD); err != nil {
		return err
	}
	if !filepath.IsAbs(r.CWD) {
		return fmt.Errorf("acp: cwd must be absolute: %q", r.CWD)
	}
	if len(r.AdditionalDirectories) > MaxCollectionItems {
		return tooMany("additionalDirectories", len(r.AdditionalDirectories))
	}
	for i, dir := range r.AdditionalDirectories {
		if strings.TrimSpace(dir) == "" {
			return required(fmt.Sprintf("additionalDirectories[%d]", i))
		}
		if err := bounded(fmt.Sprintf("additionalDirectories[%d]", i), dir, MaxTextBytes); err != nil {
			return err
		}
		if err := noNUL(fmt.Sprintf("additionalDirectories[%d]", i), dir); err != nil {
			return err
		}
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("acp: additionalDirectories[%d] must be absolute: %q", i, dir)
		}
	}
	if r.MCPServers == nil {
		return required("mcpServers")
	}
	if len(r.MCPServers) > MaxCollectionItems {
		return tooMany("mcpServers", len(r.MCPServers))
	}
	for i := range r.MCPServers {
		if err := r.MCPServers[i].Validate(); err != nil {
			return fmt.Errorf("acp: mcpServers[%d]: %w", i, err)
		}
	}
	return nil
}

func (r NewSessionResponse) Validate() error { return validateID("sessionId", string(r.SessionID)) }

func (s MCPServer) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return required("name")
	}
	if err := bounded("name", s.Name, MaxNameBytes); err != nil {
		return err
	}
	transport := s.Type
	if transport == "" {
		transport = MCPTransportStdio
	}
	switch transport {
	case MCPTransportStdio:
		if strings.TrimSpace(s.Command) == "" {
			return required("command")
		}
		if !filepath.IsAbs(s.Command) {
			return fmt.Errorf("stdio command must be absolute: %q", s.Command)
		}
		if err := bounded("command", s.Command, MaxTextBytes); err != nil {
			return err
		}
		if err := noNUL("command", s.Command); err != nil {
			return err
		}
		if s.Args == nil {
			return required("args")
		}
		if s.Env == nil {
			return required("env")
		}
		if len(s.Args) > MaxCollectionItems {
			return tooMany("args", len(s.Args))
		}
		if len(s.Env) > MaxCollectionItems {
			return tooMany("env", len(s.Env))
		}
		for i, arg := range s.Args {
			if err := bounded(fmt.Sprintf("args[%d]", i), arg, MaxTextBytes); err != nil {
				return err
			}
			if err := noNUL(fmt.Sprintf("args[%d]", i), arg); err != nil {
				return err
			}
		}
		for i, env := range s.Env {
			if strings.TrimSpace(env.Name) == "" {
				return required(fmt.Sprintf("env[%d].name", i))
			}
			if strings.ContainsRune(env.Name, '=') {
				return fmt.Errorf("env[%d].name must not contain '='", i)
			}
			if err := noNUL(fmt.Sprintf("env[%d].name", i), env.Name); err != nil {
				return err
			}
			if err := bounded(fmt.Sprintf("env[%d].value", i), env.Value, MaxTextBytes); err != nil {
				return err
			}
			if err := noNUL(fmt.Sprintf("env[%d].value", i), env.Value); err != nil {
				return err
			}
		}
	case MCPTransportHTTP, MCPTransportSSE:
		if strings.TrimSpace(s.URL) == "" {
			return required("url")
		}
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s url must be an absolute HTTP(S) URL: %q", transport, s.URL)
		}
		if s.Headers == nil {
			return required("headers")
		}
		if len(s.Headers) > MaxCollectionItems {
			return tooMany("headers", len(s.Headers))
		}
		for i, h := range s.Headers {
			if strings.TrimSpace(h.Name) == "" {
				return required(fmt.Sprintf("headers[%d].name", i))
			}
			if strings.ContainsAny(h.Name, "\r\n") || strings.ContainsAny(h.Value, "\r\n") {
				return fmt.Errorf("headers[%d] contains a newline", i)
			}
		}
	default:
		return fmt.Errorf("unsupported MCP transport %q", transport)
	}
	return nil
}

func (r PromptRequest) Validate() error {
	if err := validateID("sessionId", string(r.SessionID)); err != nil {
		return err
	}
	if r.Prompt == nil {
		return required("prompt")
	}
	if len(r.Prompt) > MaxCollectionItems {
		return tooMany("prompt", len(r.Prompt))
	}
	for i := range r.Prompt {
		if err := r.Prompt[i].Validate(); err != nil {
			return fmt.Errorf("acp: prompt[%d]: %w", i, err)
		}
	}
	return nil
}

func (r PromptResponse) Validate() error {
	switch r.StopReason {
	case StopReasonEndTurn, StopReasonMaxTokens, StopReasonMaxTurnRequests, StopReasonRefusal, StopReasonCancelled:
		return nil
	default:
		return fmt.Errorf("acp: unsupported stopReason %q", r.StopReason)
	}
}

func (b ContentBlock) Validate() error {
	switch b.Type {
	case ContentTypeText:
		return bounded("text", b.Text, MaxTextBytes)
	case ContentTypeImage, ContentTypeAudio:
		if b.Data == "" {
			return required("data")
		}
		if b.MIMEType == "" {
			return required("mimeType")
		}
		if err := bounded("data", b.Data, MaxBinaryDataBytes); err != nil {
			return err
		}
		if !validBase64Data(b.Data) {
			return fmt.Errorf("acp: data must be base64 encoded")
		}
		return nil
	case ContentTypeResourceLink:
		if b.URI == "" {
			return required("uri")
		}
		if b.Name == "" {
			return required("name")
		}
		if b.Size != nil && *b.Size < 0 {
			return fmt.Errorf("acp: size must not be negative")
		}
		return bounded("description", b.Description, MaxTextBytes)
	case ContentTypeResource:
		if b.Resource == nil {
			return required("resource")
		}
		return b.Resource.Validate()
	default:
		return fmt.Errorf("acp: unsupported content type %q", b.Type)
	}
}

func (r EmbeddedResource) Validate() error {
	if r.URI == "" {
		return required("resource.uri")
	}
	if (r.Text == nil) == (r.Blob == nil) {
		return fmt.Errorf("acp: resource must contain exactly one of text or blob")
	}
	if r.Text != nil {
		return bounded("resource.text", *r.Text, MaxTextBytes)
	}
	return bounded("resource.blob", *r.Blob, MaxBinaryDataBytes)
}

func (n SessionUpdateNotification) Validate() error {
	if err := validateID("sessionId", string(n.SessionID)); err != nil {
		return err
	}
	return n.Update.Validate()
}

func (u SessionUpdate) Validate() error {
	if !knownUpdateKind(u.Kind) {
		return fmt.Errorf("acp: unsupported session update %q", u.Kind)
	}
	var err error
	switch u.Kind {
	case UpdateUserMessageChunk, UpdateAgentMessageChunk, UpdateAgentThoughtChunk:
		if u.ContentChunk == nil {
			return required("content chunk")
		}
		err = u.ContentChunk.Content.Validate()
	case UpdateToolCall:
		if u.ToolCall == nil {
			return required("tool call")
		}
		err = u.ToolCall.validate(false)
	case UpdateToolCallUpdate:
		if u.ToolCallUpdate == nil {
			return required("tool call update")
		}
		err = u.ToolCallUpdate.validate()
	case UpdatePlan:
		if u.Plan == nil {
			return required("plan")
		}
		if u.Plan.Entries == nil {
			return required("plan.entries")
		}
		if len(u.Plan.Entries) > MaxCollectionItems {
			return tooMany("plan.entries", len(u.Plan.Entries))
		}
		for i, entry := range u.Plan.Entries {
			if entry.Content == "" {
				return required(fmt.Sprintf("plan.entries[%d].content", i))
			}
			if e := bounded(fmt.Sprintf("plan.entries[%d].content", i), entry.Content, MaxTextBytes); e != nil {
				return e
			}
			switch entry.Priority {
			case PlanPriorityHigh, PlanPriorityMedium, PlanPriorityLow:
			default:
				return fmt.Errorf("acp: plan.entries[%d].priority: unsupported %q", i, entry.Priority)
			}
			switch entry.Status {
			case PlanEntryPending, PlanEntryInProgress, PlanEntryCompleted:
			default:
				return fmt.Errorf("acp: plan.entries[%d].status: unsupported %q", i, entry.Status)
			}
		}
	case UpdateAvailableCommands:
		if u.AvailableCommands == nil {
			return required("available commands")
		}
		if u.AvailableCommands.AvailableCommands == nil {
			return required("availableCommands")
		}
		if len(u.AvailableCommands.AvailableCommands) > MaxCollectionItems {
			return tooMany("availableCommands", len(u.AvailableCommands.AvailableCommands))
		}
		for i, command := range u.AvailableCommands.AvailableCommands {
			if strings.TrimSpace(command.Name) == "" {
				return required(fmt.Sprintf("availableCommands[%d].name", i))
			}
			if strings.TrimSpace(command.Description) == "" {
				return required(fmt.Sprintf("availableCommands[%d].description", i))
			}
			if e := bounded(fmt.Sprintf("availableCommands[%d].description", i), command.Description, MaxTextBytes); e != nil {
				return e
			}
		}
	case UpdateCurrentMode:
		if u.CurrentMode == nil {
			return required("current mode")
		}
		err = validateID("currentModeId", u.CurrentMode.CurrentModeID)
	case UpdateConfigOption:
		if u.ConfigOption == nil {
			return required("config option")
		}
		if u.ConfigOption.ConfigOptions == nil {
			return required("configOptions")
		}
		if len(u.ConfigOption.ConfigOptions) > MaxCollectionItems {
			return tooMany("configOptions", len(u.ConfigOption.ConfigOptions))
		}
		total := 0
		for _, option := range u.ConfigOption.ConfigOptions {
			total += len(option)
		}
		if total > MaxTextBytes {
			return fmt.Errorf("acp: configOptions exceed %d bytes", MaxTextBytes)
		}
	case UpdateSessionInfo:
		if u.SessionInfo == nil {
			return required("session info")
		}
	case UpdateUsage:
		if u.Usage == nil {
			return required("usage")
		}
		if u.Usage.Cost != nil {
			if math.IsNaN(u.Usage.Cost.Amount) || math.IsInf(u.Usage.Cost.Amount, 0) {
				return fmt.Errorf("acp: cost.amount must be finite")
			}
			if strings.TrimSpace(u.Usage.Cost.Currency) == "" {
				return required("cost.currency")
			}
		}
	}
	if err != nil {
		return err
	}
	if sessionUpdateTextBytes(u) > MaxModelFacingUpdateText {
		return fmt.Errorf("acp: session update text exceeds %d bytes", MaxModelFacingUpdateText)
	}
	return nil
}

func (t ToolCall) validate(update bool) error {
	if err := validateID("toolCallId", string(t.ToolCallID)); err != nil {
		return err
	}
	if !update && strings.TrimSpace(t.Title) == "" {
		return required("title")
	}
	if err := bounded("title", t.Title, MaxNameBytes); err != nil {
		return err
	}
	if t.Kind != "" && !validToolKind(t.Kind) {
		return fmt.Errorf("acp: unsupported tool kind %q", t.Kind)
	}
	if t.Status != "" && !validToolStatus(t.Status) {
		return fmt.Errorf("acp: unsupported tool status %q", t.Status)
	}
	if len(t.RawInput) > MaxTextBytes || len(t.RawOutput) > MaxTextBytes {
		return fmt.Errorf("acp: tool raw input/output exceeds %d bytes", MaxTextBytes)
	}
	if err := validateLocations(t.Locations); err != nil {
		return err
	}
	if len(t.Content) > MaxCollectionItems {
		return tooMany("content", len(t.Content))
	}
	for i := range t.Content {
		if err := t.Content[i].Validate(); err != nil {
			return fmt.Errorf("acp: content[%d]: %w", i, err)
		}
	}
	return nil
}

func (t ToolCallUpdate) validate() error {
	if err := validateID("toolCallId", string(t.ToolCallID)); err != nil {
		return err
	}
	if t.Title != nil {
		if err := bounded("title", *t.Title, MaxNameBytes); err != nil {
			return err
		}
	}
	if t.Kind != nil && !validToolKind(*t.Kind) {
		return fmt.Errorf("acp: unsupported tool kind %q", *t.Kind)
	}
	if t.Status != nil && !validToolStatus(*t.Status) {
		return fmt.Errorf("acp: unsupported tool status %q", *t.Status)
	}
	if len(t.RawInput) > MaxTextBytes || len(t.RawOutput) > MaxTextBytes {
		return fmt.Errorf("acp: tool raw input/output exceeds %d bytes", MaxTextBytes)
	}
	if t.Locations != nil {
		if err := validateLocations(*t.Locations); err != nil {
			return err
		}
	}
	if t.Content != nil {
		if len(*t.Content) > MaxCollectionItems {
			return tooMany("content", len(*t.Content))
		}
		for i := range *t.Content {
			if err := (*t.Content)[i].Validate(); err != nil {
				return fmt.Errorf("acp: content[%d]: %w", i, err)
			}
		}
	}
	return nil
}

func validateLocations(locations []ToolCallLocation) error {
	if len(locations) > MaxCollectionItems {
		return tooMany("locations", len(locations))
	}
	for i, location := range locations {
		field := fmt.Sprintf("locations[%d].path", i)
		if strings.TrimSpace(location.Path) == "" {
			return required(field)
		}
		if err := bounded(field, location.Path, MaxTextBytes); err != nil {
			return err
		}
		if !filepath.IsAbs(location.Path) {
			return fmt.Errorf("acp: %s must be absolute: %q", field, location.Path)
		}
		if strings.ContainsRune(location.Path, 0) {
			return fmt.Errorf("acp: %s contains NUL", field)
		}
	}
	return nil
}

func (c ToolCallContent) Validate() error {
	switch c.Type {
	case ToolCallContentBlock:
		if c.Content == nil {
			return required("content")
		}
		return c.Content.Validate()
	case ToolCallContentDiff:
		if !filepath.IsAbs(c.Path) {
			return fmt.Errorf("acp: diff path must be absolute: %q", c.Path)
		}
		if c.OldText != nil {
			if err := bounded("oldText", *c.OldText, MaxTextBytes); err != nil {
				return err
			}
		}
		return bounded("newText", c.NewText, MaxTextBytes)
	case ToolCallContentTerminal:
		return validateID("terminalId", c.TerminalID)
	default:
		return fmt.Errorf("acp: unsupported tool call content type %q", c.Type)
	}
}

func (n CancelNotification) Validate() error  { return validateID("sessionId", string(n.SessionID)) }
func (r CloseSessionRequest) Validate() error { return validateID("sessionId", string(r.SessionID)) }

func (r RequestPermissionRequest) Validate() error {
	if err := validateID("sessionId", string(r.SessionID)); err != nil {
		return err
	}
	if err := r.ToolCall.validate(); err != nil {
		return fmt.Errorf("acp: toolCall: %w", err)
	}
	if r.Options == nil || len(r.Options) == 0 {
		return required("options")
	}
	if len(r.Options) > MaxCollectionItems {
		return tooMany("options", len(r.Options))
	}
	seen := make(map[string]struct{}, len(r.Options))
	for i, option := range r.Options {
		if err := validateID(fmt.Sprintf("options[%d].optionId", i), option.OptionID); err != nil {
			return err
		}
		if strings.TrimSpace(option.Name) == "" {
			return required(fmt.Sprintf("options[%d].name", i))
		}
		switch option.Kind {
		case PermissionAllowOnce, PermissionAllowAlways, PermissionRejectOnce, PermissionRejectAlways:
		default:
			return fmt.Errorf("acp: options[%d].kind: unsupported %q", i, option.Kind)
		}
		if _, ok := seen[option.OptionID]; ok {
			return fmt.Errorf("acp: duplicate permission option %q", option.OptionID)
		}
		seen[option.OptionID] = struct{}{}
	}
	return nil
}

func (r RequestPermissionResponse) Validate() error {
	switch r.Outcome.Outcome {
	case PermissionOutcomeCancelled:
		if r.Outcome.OptionID != "" {
			return fmt.Errorf("acp: cancelled permission outcome must not select an option")
		}
		return nil
	case PermissionOutcomeSelected:
		return validateID("outcome.optionId", r.Outcome.OptionID)
	default:
		return fmt.Errorf("acp: unsupported permission outcome %q", r.Outcome.Outcome)
	}
}

func validateID(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return required(field)
	}
	return bounded(field, value, MaxIdentifierBytes)
}
func required(field string) error { return fmt.Errorf("acp: %s is required", field) }

func noNUL(field, value string) error {
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("acp: %s contains NUL", field)
	}
	return nil
}

// validBase64Data reports whether value decodes as standard base64, with or
// without padding; agents disagree on padding, so both encodings are accepted.
// The decoded bytes are discarded: validation never retains a copy.
func validBase64Data(value string) bool {
	if _, err := base64.StdEncoding.DecodeString(value); err == nil {
		return true
	}
	_, err := base64.RawStdEncoding.DecodeString(value)
	return err == nil
}
func bounded(field, value string, max int) error {
	if len(value) > max {
		return fmt.Errorf("acp: %s exceeds %d bytes", field, max)
	}
	return nil
}
func tooMany(field string, got int) error {
	return fmt.Errorf("acp: %s has %d items; maximum is %d", field, got, MaxCollectionItems)
}

func validToolKind(kind ToolKind) bool {
	switch kind {
	case ToolKindRead, ToolKindEdit, ToolKindDelete, ToolKindMove, ToolKindSearch,
		ToolKindExecute, ToolKindThink, ToolKindFetch, ToolKindSwitchMode, ToolKindOther:
		return true
	default:
		return false
	}
}

func validToolStatus(status ToolCallStatus) bool {
	switch status {
	case ToolCallPending, ToolCallInProgress, ToolCallCompleted, ToolCallFailed:
		return true
	default:
		return false
	}
}
