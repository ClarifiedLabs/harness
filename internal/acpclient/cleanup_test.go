package acpclient

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/agentsession"
	"harness/internal/background"
	"harness/internal/mcp/jsonrpc"
)

// This agent deliberately ignores EOF but cooperates with TERM, as a delegate
// that must tear down its own separately grouped LSP/MCP children would.
func TestACPCleanupProcess(t *testing.T) {
	if os.Getenv("HARNESS_ACPCLIENT_CLEANUP_HELPER") != "1" {
		return
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	a := helperAgent{dec: jsonrpc.NewDecoder(os.Stdin), enc: jsonrpc.NewEncoder(os.Stdout), logPath: os.Getenv("HARNESS_ACPCLIENT_LOG")}
	initialize, err := a.request(acp.MethodInitialize)
	if err != nil {
		os.Exit(10)
	}
	_ = a.respond(*initialize.ID, acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersion})
	newSession, err := a.request(acp.MethodSessionNew)
	if err != nil {
		os.Exit(11)
	}
	_ = a.respond(*newSession.ID, acp.NewSessionResponse{SessionID: "remote-session"})
	prompt, err := a.prompt("open")
	if err != nil {
		os.Exit(12)
	}
	_ = a.respond(*prompt.ID, acp.PromptResponse{StopReason: acp.StopReasonEndTurn})
	_, _ = io.Copy(io.Discard, os.Stdin)
	<-term
	a.log("terminated-cleanly")
	os.Exit(0)
}

func TestManagerCloseAllJoinsACPAfterCanceledClose(t *testing.T) {
	opts, logPath := testOptions(t, "cleanup")
	opts.Argv = []string{os.Args[0], "-test.run=^TestACPCleanupProcess$"}
	opts.Env = append(opts.Env, "HARNESS_ACPCLIENT_CLEANUP_HELPER=1")
	opts.ReapTimeout = 0 // Exercise the production EOF/TERM cleanup budget.
	jobs := background.NewManager(background.Options{})
	manager := agentsession.NewManager(agentsession.Options{Background: jobs, Canceler: jobs, CloseTimeout: 20 * time.Millisecond})
	opened := make(chan *Runtime, 1)
	start, err := manager.Start(context.Background(), agentsession.StartRequest{
		Kind: "acp", Prompt: "open", Factory: func(ctx context.Context, info agentsession.SessionInfo) (agentsession.Runtime, error) {
			runtime, err := Open(ctx, info, opts)
			if err == nil {
				opened <- runtime
			}
			return runtime, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var runtime *Runtime
	select {
	case runtime = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("ACP did not open")
	}
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
		jobs.ShutdownAndWait(time.Second)
	})
	for {
		changed := manager.Changed()
		snapshot, _ := manager.Get(start.Session.ID)
		if snapshot.State == agentsession.StateIdle {
			break
		}
		select {
		case <-changed:
		case <-time.After(5 * time.Second):
			t.Fatalf("ACP did not become idle: %+v", snapshot)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtime.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled interactive Close = %v", err)
	}
	select {
	case <-runtime.CleanupDone():
		t.Fatal("cleanup completed before the EOF-ignoring target received TERM")
	default:
	}
	closed := make(chan error, 1)
	go func() { closed <- manager.CloseAll(ctx) }()
	select {
	case <-closed: // Close may report its timeout; process cleanup must be joined.
	case <-time.After(25 * time.Second):
		t.Fatal("CloseAll did not finish bounded ACP cleanup")
	}
	select {
	case <-runtime.CleanupDone():
	default:
		t.Fatal("CloseAll returned before ACP cleanup completed")
	}
	select {
	case <-runtime.child.Done():
	default:
		t.Fatal("CloseAll returned before reaping ACP child")
	}
	if methods := readMethods(t, logPath); !slices.Contains(methods, "terminated-cleanly") {
		t.Fatalf("target was killed before its TERM cleanup: %v", methods)
	}
}
