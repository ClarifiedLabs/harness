package mcpproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"harness/internal/apikey"
	"harness/internal/logging"
	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
	"harness/internal/metrics"
	"harness/internal/tracing"
)

// pageSize is the tools/list page size. Most setups fit one page; pagination is
// implemented because the MCP spec requires cursor support and the aggregate can
// grow large.
const pageSize = 100

// qualifiedPrefix is the namespace prefix for every aggregated tool.
const qualifiedPrefix = "mcp__"

// route records where a qualified tool name dispatches.
type route struct {
	supervisor *Supervisor
	bareName   string
	tool       mcp.Tool
}

type resourceRoute struct {
	supervisor *Supervisor
	tool       string
}

type registryMetrics struct {
	requests      *metrics.Counter
	errors        *metrics.Counter
	requestBytes  *metrics.Counter
	responseBytes *metrics.Counter
	duration      *metrics.Counter
}

func registerRegistryMetrics(reg *metrics.Registry) *registryMetrics {
	if reg == nil {
		return nil
	}
	return &registryMetrics{
		requests:      reg.Counter("mcp_proxy_requests_total", "MCP proxy tool requests."),
		errors:        reg.Counter("mcp_proxy_errors_total", "MCP proxy tool request errors."),
		requestBytes:  reg.Counter("mcp_proxy_request_bytes_total", "MCP proxy tool argument bytes."),
		responseBytes: reg.Counter("mcp_proxy_response_bytes_total", "MCP proxy tool result bytes."),
		duration:      reg.Counter("mcp_proxy_request_duration_seconds_total", "MCP proxy tool request duration in seconds."),
	}
}

// Registry aggregates the tools of every supervised server into one namespaced
// surface and implements mcp.ToolProvider for the proxy's server sessions. It
// maintains a merged sorted tool list (names rewritten to mcp__<server>__<tool>)
// and a reverse route map, both rebuilt whenever any supervisor's tools change.
// It fans tools/list_changed out to subscribed sessions.
type Registry struct {
	supervisors []*Supervisor
	logger      *slog.Logger
	metrics     *registryMetrics

	mu             sync.RWMutex
	tools          []mcp.Tool       // merged, namespaced, sorted by name
	routes         map[string]route // qualified name -> dispatch target
	resourceRoutes map[string][]resourceRoute

	sessions map[*mcp.ServerSession]struct{}
}

// NewRegistry builds a registry over servers, injecting an onToolsChanged
// callback into each so a supervisor's tool change rebuilds the table and
// broadcasts list_changed. It builds the initial table immediately.
func NewRegistry(servers []*Supervisor, logger *slog.Logger) *Registry {
	return newRegistryWithMetrics(servers, logger, nil)
}

func newRegistryWithMetrics(servers []*Supervisor, logger *slog.Logger, collectors *registryMetrics) *Registry {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	r := &Registry{
		supervisors:    servers,
		logger:         logger,
		metrics:        collectors,
		routes:         map[string]route{},
		resourceRoutes: map[string][]resourceRoute{},
		sessions:       map[*mcp.ServerSession]struct{}{},
	}
	for _, s := range servers {
		s.onToolsChanged = r.onSupervisorToolsChanged
	}
	r.rebuild()
	return r
}

// onSupervisorToolsChanged is the callback injected into each supervisor. It
// rebuilds the aggregated table then broadcasts list_changed to subscribers.
func (r *Registry) onSupervisorToolsChanged() {
	r.rebuild()
	r.BroadcastListChanged()
}

// rebuild recomputes the merged namespaced tool list and the reverse route map
// under the write lock. Per-server exclusions match bare names so excluded tools
// are neither listed nor routable, including after refreshes. Qualified names
// that collide with advertised excluded tools are also omitted.
// Tools whose qualified name is not provider-safe are dropped with a warning
// (never rewritten/truncated — a truncated name could collide and break routing).
func (r *Registry) rebuild() {
	// Serialize snapshots as well as publication: an older concurrent rebuild
	// must not restore a route removed by a newer exclusion-bearing snapshot.
	r.mu.Lock()
	defer r.mu.Unlock()

	var tools []mcp.Tool
	routes := map[string]route{}
	excluded := map[string]bool{}

	for _, sup := range r.supervisors {
		for _, t := range sup.Tools() {
			qualified := qualifiedPrefix + sup.Name() + "__" + t.Name
			if slices.Contains(sup.cfg.ExcludedTools, t.Name) {
				excluded[qualified] = true
				continue
			}
			if !serverNameRE.MatchString(qualified) {
				r.logger.Warn("tool omitted: qualified name not provider-safe",
					logging.Category(categoryGate), "server", sup.Name(), "tool", t.Name, "qualified", qualified)
				continue
			}
			if _, dup := routes[qualified]; dup {
				// Names containing __ can collide after qualification. Keep the
				// first route unless an exclusion blocks this name below.
				r.logger.Warn("tool omitted: duplicate qualified name",
					logging.Category(categoryGate), "qualified", qualified)
				continue
			}
			nt := t
			nt.Name = qualified
			tools = append(tools, nt)
			routes[qualified] = route{supervisor: sup, bareName: t.Name, tool: nt}
		}
	}

	// Do not let an excluded identity redirect to a different server's tool,
	// regardless of supervisor order. Only advertised exclusions reserve names.
	tools = slices.DeleteFunc(tools, func(t mcp.Tool) bool {
		if !excluded[t.Name] {
			return false
		}
		delete(routes, t.Name)
		r.logger.Warn("tool omitted: qualified name collides with excluded tool",
			logging.Category(categoryGate), "qualified", t.Name)
		return true
	})

	slices.SortFunc(tools, func(a, b mcp.Tool) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})

	resourceRoutes := map[string][]resourceRoute{}
	for _, t := range tools {
		uri, _ := toolUIMetadata(t)
		if uri == "" {
			continue
		}
		rt := routes[t.Name]
		resourceRoutes[uri] = append(resourceRoutes[uri], resourceRoute{supervisor: rt.supervisor, tool: t.Name})
	}

	r.tools = tools
	r.routes = routes
	r.resourceRoutes = resourceRoutes
}

// ListTools returns one page of the merged namespaced list, paginated by an
// opaque base64 cursor (the next index). An invalid cursor is a CodeInvalidParams
// jsonrpc error.
func (r *Registry) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	start, err := decodeCursor(cursor)
	if err != nil {
		return mcp.ListToolsResult{}, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "invalid cursor")
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if start > len(r.tools) {
		return mcp.ListToolsResult{}, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "invalid cursor")
	}

	end := min(start+pageSize, len(r.tools))
	page := slices.Clone(r.tools[start:end])
	result := mcp.ListToolsResult{Tools: page}
	if end < len(r.tools) {
		result.NextCursor = encodeCursor(end)
	}
	return result, nil
}

// ListResources returns a valid empty page. MCP Apps UI resources are discovered
// through tool metadata and need not appear in a downstream resources/list.
func (r *Registry) ListResources(ctx context.Context, cursor string) (mcp.ListResourcesResult, error) {
	if cursor != "" {
		return mcp.ListResourcesResult{}, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "invalid cursor")
	}
	return mcp.ListResourcesResult{Resources: []mcp.Resource{}}, nil
}

// ReadResource routes a UI resource read using an optional namespaced sourceTool
// to disambiguate identical URIs advertised by different downstream servers.
func (r *Registry) ReadResource(ctx context.Context, params mcp.ReadResourceParams) (*mcp.ReadResourceResult, error) {
	r.mu.RLock()
	candidates := append([]resourceRoute(nil), r.resourceRoutes[params.URI]...)
	r.mu.RUnlock()

	if params.SourceTool != "" {
		for _, candidate := range candidates {
			if candidate.tool == params.SourceTool {
				return candidate.supervisor.ReadResource(ctx, params.URI)
			}
		}
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "unknown resource %q for source tool %q", params.URI, params.SourceTool)
	}
	if len(candidates) == 0 {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "unknown resource: %s", params.URI)
	}
	if len(candidates) > 1 {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "ambiguous resource %q: sourceTool is required", params.URI)
	}
	return candidates[0].supervisor.ReadResource(ctx, params.URI)
}

// toolUIMetadata extracts current and deprecated MCP Apps resource URIs.
// Visibility filtering belongs to model-facing clients, not this aggregation
// boundary: app hosts still need app-only tools and their resource routes.
func toolUIMetadata(t mcp.Tool) (uri string, modelVisible bool) {
	modelVisible = true
	if len(t.Meta) == 0 {
		return "", modelVisible
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(t.Meta, &meta) != nil {
		return "", modelVisible
	}
	if raw, ok := meta["ui"]; ok {
		var ui map[string]json.RawMessage
		if json.Unmarshal(raw, &ui) == nil {
			_ = json.Unmarshal(ui["resourceUri"], &uri)
			if visibility, ok := ui["visibility"]; ok && string(visibility) != "null" {
				var values []string
				if json.Unmarshal(visibility, &values) == nil {
					modelVisible = slices.Contains(values, "model")
				}
			}
		}
	}
	if uri == "" {
		_ = json.Unmarshal(meta["ui/resourceUri"], &uri)
	}
	return uri, modelVisible
}

// CallTool routes a qualified call to the owning supervisor and returns its
// result verbatim. An unknown tool is a CodeInvalidParams jsonrpc error (the
// model should not have called it); a known tool's result (including IsError) is
// passed straight through.
func (r *Registry) CallTool(ctx context.Context, qualified string, args json.RawMessage) (*mcp.CallToolResult, error) {
	start := time.Now()
	sup, bare, ok := r.routeForCall(ctx, qualified)
	if !ok {
		err := jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "unknown tool: %s", qualified)
		duration := time.Since(start)
		r.logToolCall(ctx, qualified, "", "", len(args), 0, duration, false, err)
		r.recordToolCall(ctx, "", "", len(args), 0, duration, false, err)
		return nil, err
	}
	result, err := sup.CallTool(ctx, bare, args)
	responseBytes := 0
	isError := false
	if result != nil {
		isError = result.IsError
		if raw, marshalErr := json.Marshal(result); marshalErr == nil {
			responseBytes = len(raw)
		}
	}
	duration := time.Since(start)
	r.logToolCall(ctx, qualified, sup.Name(), bare, len(args), responseBytes, duration, isError, err)
	r.recordToolCall(ctx, sup.Name(), bare, len(args), responseBytes, duration, isError, err)
	return result, err
}

func (r *Registry) recordToolCall(ctx context.Context, server, tool string, requestBytes, responseBytes int, duration time.Duration, isError bool, err error) {
	if r.metrics == nil {
		return
	}
	key := "anonymous"
	if name, ok := apikey.AuthorizedNameFromContext(ctx); ok {
		key = name
	}
	labels := map[string]string{"key": key}
	if server != "" {
		labels["mcp"] = server
	}
	if tool != "" {
		labels["tool"] = tool
	}
	r.metrics.requests.Inc(labels)
	r.metrics.duration.Add(duration.Seconds(), labels)
	if requestBytes != 0 {
		r.metrics.requestBytes.Add(float64(requestBytes), labels)
	}
	if responseBytes != 0 {
		r.metrics.responseBytes.Add(float64(responseBytes), labels)
	}
	if (err != nil || isError) && !errors.Is(ctx.Err(), context.Canceled) {
		r.metrics.errors.Inc(labels)
	}
}

func (r *Registry) logToolCall(ctx context.Context, qualified, server, tool string, requestBytes, responseBytes int, duration time.Duration, isError bool, err error) {
	reqInfo, _ := mcp.RequestInfoFromContext(ctx)
	attrs := []any{
		"requester", reqInfo.Requester,
		"requester_version", reqInfo.RequesterVersion,
		"remote_addr", reqInfo.RemoteAddr,
		"mcp", server,
		"tool", tool,
		"qualified_tool", qualified,
		"request_bytes", requestBytes,
		"response_bytes", responseBytes,
		"duration", duration,
		"is_error", isError,
	}
	if tc, ok := tracing.TraceFromContext(ctx); ok {
		attrs = append(attrs, tracing.LogAttrs(tc)...)
	} else if reqInfo.Trace.TraceID != "" {
		attrs = append(attrs, tracing.LogAttrs(reqInfo.Trace)...)
	}
	if err != nil {
		attrs = append(attrs, "err", err)
		r.logger.Warn("mcp tool request completed", attrs...)
		return
	}
	r.logger.Info("mcp tool request completed", attrs...)
}

// routeForCall enforces same-server app calls using exact route records, never
// string parsing. Bare companion names resolve only on the source supervisor.
func (r *Registry) routeForCall(ctx context.Context, requested string) (*Supervisor, string, bool) {
	source := mcp.SourceToolFromContext(ctx)
	if source == "" {
		return r.route(requested)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	sourceRoute, ok := r.routes[source]
	if !ok {
		return nil, "", false
	}
	if target, exact := r.routes[requested]; exact {
		if target.supervisor != sourceRoute.supervisor || !visibleToApp(target.tool) {
			return nil, "", false
		}
		return target.supervisor, target.bareName, true
	}
	var target route
	found := false
	for _, candidate := range r.routes {
		if candidate.supervisor == sourceRoute.supervisor && candidate.bareName == requested {
			if found {
				return nil, "", false
			}
			target = candidate
			found = true
		}
	}
	if !found || !visibleToApp(target.tool) {
		return nil, "", false
	}
	return target.supervisor, target.bareName, true
}

func visibleToApp(t mcp.Tool) bool {
	if len(t.Meta) == 0 {
		return true
	}
	var meta struct {
		UI struct {
			Visibility json.RawMessage `json:"visibility"`
		} `json:"ui"`
	}
	if err := json.Unmarshal(t.Meta, &meta); err != nil || len(meta.UI.Visibility) == 0 {
		return true
	}
	var visibility []string
	if err := json.Unmarshal(meta.UI.Visibility, &visibility); err != nil {
		return false
	}
	return slices.Contains(visibility, "app")
}

// route resolves a qualified name to its supervisor and bare tool name via the
// reverse map (no string-splitting, so server names may contain underscores).
func (r *Registry) route(qualified string) (*Supervisor, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.routes[qualified]
	if !ok {
		return nil, "", false
	}
	return rt.supervisor, rt.bareName, true
}

// Subscribe registers a session for tools/list_changed fan-out.
func (r *Registry) Subscribe(s *mcp.ServerSession) {
	r.mu.Lock()
	r.sessions[s] = struct{}{}
	r.mu.Unlock()
}

// Unsubscribe stops fan-out to a session (called when its Serve returns).
func (r *Registry) Unsubscribe(s *mcp.ServerSession) {
	r.mu.Lock()
	delete(r.sessions, s)
	r.mu.Unlock()
}

// BroadcastListChanged notifies every subscribed session that the tool list
// changed. Dead or backpressured sessions are dropped so one blocked client
// cannot wedge supervisor callbacks.
func (r *Registry) BroadcastListChanged() {
	r.mu.RLock()
	sessions := make([]*mcp.ServerSession, 0, len(r.sessions))
	for s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.mu.RUnlock()

	var dead []*mcp.ServerSession
	for _, s := range sessions {
		select {
		case <-s.Done():
			dead = append(dead, s)
			continue
		default:
		}
		if err := s.TryNotifyToolsListChanged(); err != nil {
			dead = append(dead, s)
		}
	}

	if len(dead) > 0 {
		r.mu.Lock()
		for _, s := range dead {
			delete(r.sessions, s)
		}
		r.mu.Unlock()
	}
}

// encodeCursor encodes a next-index as an opaque base64 string.
func encodeCursor(idx int) string {
	return base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(idx)))
}

// decodeCursor decodes a cursor to its index. An empty cursor is index 0. A
// non-base64 or non-numeric/negative cursor is an error.
func decodeCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, err
	}
	idx, err := strconv.Atoi(string(raw))
	if err != nil {
		return 0, err
	}
	if idx < 0 {
		return 0, strconv.ErrRange
	}
	return idx, nil
}
