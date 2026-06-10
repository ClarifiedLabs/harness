package mcpproxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
)

// TestHelperProcess is not a real test: when invoked with the sentinel env var
// HELPER_MODE=mcp it runs a tiny newline-JSON-RPC MCP server on stdin/stdout,
// configured by env vars. The supervisor and daemon tests spawn it via
// exec.Command(os.Args[0], "-test.run=TestHelperProcess$", ...) with that env,
// the canonical stdlib fake-subprocess idiom (no separate binary, stays
// stdlib-only).
//
// Configuration env vars:
//
//	HELPER_MODE=mcp          activate the helper (otherwise it returns)
//	HELPER_TOOLS=a,b,c       tool names to advertise (default: "echo")
//	HELPER_EXIT_AFTER_CALLS  exit(0) after N successful tools/call (0 = never)
//	HELPER_EMIT_LIST_CHANGED after the first tools/call, send tools/list_changed
//	                         and switch the advertised tool set to HELPER_TOOLS2
//	HELPER_TOOLS2=x,y        the post-list_changed tool set
//	HELPER_STDERR=line       write this line to stderr at startup
//	HELPER_STDERR_BURST=n    write n newline-free bytes to stderr at startup (to
//	                         overflow the proxy's 1 MB scanner buffer)
//	HELPER_BAD_VERSION       respond to initialize with an unsupported version
//	HELPER_HANG_NO_INIT      start and never answer initialize; ignores stdin
//	HELPER_FAIL_LIST         return a JSON-RPC error for tools/list
//	HELPER_NO_TOOLS_CAP      omit the tools capability from initialize
//	HELPER_SPAWN_COUNTER=path increment a counter file at startup (spawn count)
func TestHelperProcess(t *testing.T) {
	if os.Getenv("HELPER_MODE") != "mcp" {
		return
	}
	runHelperServer()
	// runHelperServer exits the process; this is never reached.
}

// helperServer is the mutable state of the fake server.
type helperServer struct {
	enc       *jsonrpc.Encoder
	tools     []mcp.Tool
	calls     int
	exitAfter int
	emitLC    bool
	tools2    []mcp.Tool
	lcSent    bool
}

func runHelperServer() {
	if line := os.Getenv("HELPER_STDERR"); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}
	// A single newline-free burst larger than the proxy's 1 MB scanner buffer:
	// the scanner errors out, and the proxy must fall back to draining (else the
	// child blocks on write once the pipe buffer fills, wedging its tool calls).
	if n := atoiOr(os.Getenv("HELPER_STDERR_BURST"), 0); n > 0 {
		burst := make([]byte, n)
		for i := range burst {
			burst[i] = 'x'
		}
		_, _ = os.Stderr.Write(burst)
	}
	if cf := os.Getenv("HELPER_SPAWN_COUNTER"); cf != "" {
		bumpCounter(cf)
	}
	if os.Getenv("HELPER_HANG_NO_INIT") != "" {
		select {}
	}

	s := &helperServer{
		enc:       jsonrpc.NewEncoder(os.Stdout),
		tools:     toolsFromEnv(os.Getenv("HELPER_TOOLS"), "echo"),
		tools2:    toolsFromEnv(os.Getenv("HELPER_TOOLS2"), ""),
		exitAfter: atoiOr(os.Getenv("HELPER_EXIT_AFTER_CALLS"), 0),
		emitLC:    os.Getenv("HELPER_EMIT_LIST_CHANGED") != "",
	}

	dec := jsonrpc.NewDecoder(os.Stdin)
	for {
		msg, err := dec.Decode()
		if err != nil {
			os.Exit(0)
		}
		s.handle(msg)
	}
}

func (s *helperServer) handle(msg jsonrpc.Message) {
	switch msg.Method {
	case mcp.MethodInitialize:
		s.handleInitialize(msg)
	case mcp.NotifInitialized:
		// no response
	case mcp.MethodPing:
		s.reply(msg, json.RawMessage(`{}`))
	case mcp.MethodListTools:
		s.handleList(msg)
	case mcp.MethodCallTool:
		s.handleCall(msg)
	default:
		if msg.Kind() == jsonrpc.KindRequest {
			s.replyErr(msg, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method not found: %s", msg.Method))
		}
	}
}

func (s *helperServer) handleInitialize(msg jsonrpc.Message) {
	version := mcp.ProtocolVersion
	if os.Getenv("HELPER_BAD_VERSION") != "" {
		version = "1999-01-01"
	}
	caps := mcp.ServerCapabilities{}
	if os.Getenv("HELPER_NO_TOOLS_CAP") == "" {
		caps.Tools = &mcp.ToolsCapability{ListChanged: true}
	}
	res := mcp.InitializeResult{
		ProtocolVersion: version,
		Capabilities:    caps,
		ServerInfo:      mcp.Implementation{Name: "helper", Version: "1.0"},
	}
	s.reply(msg, mustJSON(res))
}

func (s *helperServer) handleList(msg jsonrpc.Message) {
	if os.Getenv("HELPER_FAIL_LIST") != "" {
		s.replyErr(msg, jsonrpc.Errorf(jsonrpc.CodeInternal, "forced tools/list failure"))
		return
	}
	res := mcp.ListToolsResult{Tools: s.tools}
	s.reply(msg, mustJSON(res))
}

func (s *helperServer) handleCall(msg jsonrpc.Message) {
	var p mcp.CallToolParams
	_ = json.Unmarshal(msg.Params, &p)

	// Echo the arguments back as a text content block so round-trips are visible.
	res := mcp.CallToolResult{
		Content: []mcp.ContentBlock{{Type: "text", Text: string(p.Arguments)}},
	}
	s.reply(msg, mustJSON(res))
	s.calls++

	if s.emitLC && !s.lcSent {
		s.lcSent = true
		s.tools = s.tools2
		_ = s.enc.Encode(jsonrpc.NewNotification(mcp.NotifToolsListChanged, json.RawMessage(`{}`)))
	}

	if s.exitAfter > 0 && s.calls >= s.exitAfter {
		os.Exit(0)
	}
}

func (s *helperServer) reply(req jsonrpc.Message, result json.RawMessage) {
	if req.ID == nil {
		return
	}
	_ = s.enc.Encode(jsonrpc.NewResponse(*req.ID, result))
}

func (s *helperServer) replyErr(req jsonrpc.Message, e *jsonrpc.Error) {
	if req.ID == nil {
		return
	}
	_ = s.enc.Encode(jsonrpc.NewErrorResponse(*req.ID, e))
}

func toolsFromEnv(csv, dflt string) []mcp.Tool {
	if csv == "" {
		csv = dflt
	}
	if csv == "" {
		return nil
	}
	var out []mcp.Tool
	for name := range strings.SplitSeq(csv, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = append(out, mcp.Tool{
			Name:        name,
			Description: "tool " + name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func atoiOr(s string, dflt int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return dflt
}

// bumpCounter atomically increments a small integer in a file (spawn counter).
// It is racy under heavy concurrency but the tests spawn serially enough that a
// read-modify-write is adequate; a shared child is spawned exactly once.
func bumpCounter(path string) {
	data, _ := os.ReadFile(path)
	n := atoiOr(strings.TrimSpace(string(data)), 0)
	_ = os.WriteFile(path, []byte(strconv.Itoa(n+1)), 0o600)
}

// readCounter reads a spawn counter file written by bumpCounter.
func readCounter(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return atoiOr(strings.TrimSpace(string(data)), 0)
}
