package subscription

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestTransportClonedAfterHTTP2Initialization(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warmup" {
			if r.ProtoMajor != 2 {
				t.Errorf("warmup protocol = %s, want HTTP/2", r.Proto)
			}
		} else if r.ProtoMajor != 1 {
			t.Errorf("quota protocol = %s, want HTTP/1", r.Proto)
		}
		_, _ = io.WriteString(w, `{"plan_type":"plus"}`)
	}))
	server.EnableHTTP2 = true
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // httptest certificate only
	transport.ForceAttemptHTTP2 = true
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "https://"))
	}
	defer transport.CloseIdleConnections()
	warm := &http.Client{Transport: transport}
	resp, err := warm.Get(server.URL + "/warmup")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	before := append([]string(nil), transport.TLSClientConfig.NextProtos...)
	client := New(Options{Client: warm})
	defer client.http.CloseIdleConnections()
	account, err := client.Resolve(context.Background(), config("openai-codex"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := account.Status(context.Background()); err != nil {
		t.Fatalf("quota request after HTTP/2 initialization: %v", err)
	}
	if !reflect.DeepEqual(before, transport.TLSClientConfig.NextProtos) {
		t.Fatal("quota client mutated caller's ALPN configuration")
	}
}
