package metrics

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func boolPointer(v bool) *bool { return &v }

func TestResolve(t *testing.T) {
	tests := []struct {
		name      string
		config    Config
		overrides Overrides
		want      Settings
	}{
		{name: "defaults", want: Settings{Enabled: true, Listen: "127.0.0.1:9090"}},
		{name: "config enabled", config: Config{Enabled: boolPointer(true)}, want: Settings{Enabled: true, Listen: "127.0.0.1:9090"}},
		{name: "config disabled", config: Config{Enabled: boolPointer(false)}, want: Settings{Enabled: false, Listen: "127.0.0.1:9090"}},
		{name: "disable flag", config: Config{Enabled: boolPointer(true)}, overrides: Overrides{Disable: true, DisableSet: true}, want: Settings{Enabled: false, Listen: "127.0.0.1:9090"}},
		{name: "explicit false disable flag", config: Config{Enabled: boolPointer(false)}, overrides: Overrides{DisableSet: true}, want: Settings{Enabled: true, Listen: "127.0.0.1:9090"}},
		{name: "unset disable flag preserves config", config: Config{Enabled: boolPointer(false)}, overrides: Overrides{Disable: false}, want: Settings{Enabled: false, Listen: "127.0.0.1:9090"}},
		{name: "config listen", config: Config{Listen: "0.0.0.0:9100"}, want: Settings{Enabled: true, Listen: "0.0.0.0:9100", ListenExplicit: true}},
		{name: "flag listen beats config", config: Config{Listen: "0.0.0.0:9100"}, overrides: Overrides{Listen: "127.0.0.1:9200", ListenSet: true}, want: Settings{Enabled: true, Listen: "127.0.0.1:9200", ListenExplicit: true}},
		{name: "empty flag retains default", overrides: Overrides{ListenSet: true}, want: Settings{Enabled: true, Listen: "127.0.0.1:9090", ListenExplicit: true}},
		{name: "empty flag retains config", config: Config{Listen: "0.0.0.0:9100"}, overrides: Overrides{ListenSet: true}, want: Settings{Enabled: true, Listen: "0.0.0.0:9100", ListenExplicit: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.config, "127.0.0.1:9090", tc.overrides)
			if got != tc.want {
				t.Fatalf("Resolve() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestNewWithBuildInfo(t *testing.T) {
	reg := NewWithBuildInfo(BuildInfo{Name: "service_build_info", Help: "Service build information.", Version: "v1.2.3"})
	var out strings.Builder
	reg.Render(&out)
	want := "# HELP service_build_info Service build information.\n" +
		"# TYPE service_build_info gauge\n" +
		"service_build_info{version=\"v1.2.3\"} 1\n"
	if out.String() != want {
		t.Fatalf("build info exposition:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestStartEndpointDisabledOrNilRegistryNoOp(t *testing.T) {
	ln := listenLocal(t)
	defer ln.Close()
	addr := ln.Addr().String()

	if _, err := StartEndpoint(context.Background(), nil, New(), Settings{Listen: addr, ListenExplicit: true}); err != nil {
		t.Fatalf("disabled StartEndpoint() error = %v", err)
	}
	if _, err := StartEndpoint(context.Background(), nil, nil, Settings{Enabled: true, Listen: addr, ListenExplicit: true}); err != nil {
		t.Fatalf("nil registry StartEndpoint() error = %v", err)
	}
}

func TestStartEndpointImplicitBindFailureWarns(t *testing.T) {
	ln := listenLocal(t)
	defer ln.Close()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	if _, err := StartEndpoint(context.Background(), logger, New(), Settings{Enabled: true, Listen: ln.Addr().String()}); err != nil {
		t.Fatalf("StartEndpoint() error = %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "metrics endpoint disabled (listen failed)") || !strings.Contains(got, ln.Addr().String()) {
		t.Fatalf("warning = %q", got)
	}
}

func TestStartEndpointExplicitBindFailureReturnsError(t *testing.T) {
	ln := listenLocal(t)
	defer ln.Close()
	addr := ln.Addr().String()

	_, err := StartEndpoint(context.Background(), nil, New(), Settings{Enabled: true, Listen: addr, ListenExplicit: true})
	if err == nil || !strings.Contains(err.Error(), "metrics listen "+addr+":") {
		t.Fatalf("StartEndpoint() error = %v, want metrics listen error", err)
	}
}

func TestStartEndpointServesUnauthenticatedAndStopsOnCancel(t *testing.T) {
	addr := unusedLocalAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg := NewWithBuildInfo(BuildInfo{Name: "test_build_info", Help: "test", Version: "dev"})
	if _, err := StartEndpoint(ctx, nil, reg, Settings{Enabled: true, Listen: addr, ListenExplicit: true}); err != nil {
		t.Fatalf("StartEndpoint() error = %v", err)
	}

	url := "http://" + addr + "/metrics"
	var body string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /metrics status = %d", resp.StatusCode)
			}
			body = string(data)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(body, `test_build_info{version="dev"} 1`) {
		t.Fatalf("GET /metrics body = %q", body)
	}

	cancel()
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("metrics listener %s remained open after cancellation", addr)
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func unusedLocalAddress(t *testing.T) string {
	t.Helper()
	ln := listenLocal(t)
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
