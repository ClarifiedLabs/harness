package modelbootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/modelcatalog"
)

func TestOpenMissingConfiguration(t *testing.T) {
	home := t.TempDir()
	_, err := Open(context.Background(), Options{Getenv: func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	}})
	if err == nil || !strings.Contains(err.Error(), "no model config file found") {
		t.Fatalf("error=%v", err)
	}
	_, err = Open(context.Background(), Options{ConfigPath: filepath.Join(t.TempDir(), "missing.json")})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit missing config error=%v", err)
	}
}

func TestOpenLocalNeedsNoProxyKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"provider_configs":["provider.json"],"models_dev_cache_ttl":0,"provider_models_cache_ttl":0,"subscription_poll_interval":0,"api_keys_file":"missing-keys.json","metrics":{"listen":"not-an-address"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider.json"), []byte(`{"name":"test","api_type":"openai","base_url":"https://example.invalid","api_key":"secret","models":[{"name":"test"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	service, err := Open(context.Background(), Options{ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	catalog, err := service.Backend.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Targets) == 0 {
		t.Fatal("empty catalog")
	}
	if service.Accounts == nil {
		t.Fatal("missing accounts")
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenStartupCancellationDoesNotStopMaintenance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"provider_configs":["provider.json"],"provider_models_cache_ttl":0,"subscription_poll_interval":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider.json"), []byte(`{"name":"test","api_type":"openai","base_url":"https://example.invalid","api_key":"secret","models":[{"name":"test"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	startupCtx, cancelStartup := context.WithCancel(context.Background())
	defer cancelStartup()
	started, finished := make(chan context.Context, 1), make(chan struct{})
	service, err := Open(startupCtx, Options{ConfigPath: path, Dependencies: Dependencies{ModelsDevCatalog: func(ctx context.Context) (*modelcatalog.Catalog, error) {
		started <- ctx
		<-ctx.Done()
		close(finished)
		return nil, ctx.Err()
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	var maintenanceCtx context.Context
	select {
	case maintenanceCtx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	cancelStartup()
	if err := maintenanceCtx.Err(); err != nil {
		t.Fatalf("startup cancellation stopped maintenance: %v", err)
	}
	if _, err := service.Backend.Catalog(context.Background()); err != nil {
		t.Fatalf("backend unusable after startup cancellation: %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before refresh finished")
	}
	if !errors.Is(maintenanceCtx.Err(), context.Canceled) {
		t.Fatalf("Close did not cancel maintenance: %v", maintenanceCtx.Err())
	}
}

func TestOpenCanceledStartupContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service, err := Open(ctx, Options{})
	if service != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("service=%v error=%v, want nil/context.Canceled", service, err)
	}
}

func TestOpenCancellationDuringStartupJoinsMaintenance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"provider_configs":["provider.json"],"provider_models_cache_ttl":0,"subscription_poll_interval":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider.json"), []byte(`{"name":"test","api_type":"openai","base_url":"https://example.invalid","api_key":"secret","models":[{"name":"test"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	service, err := Open(ctx, Options{ConfigPath: path, Dependencies: Dependencies{
		Now: func() time.Time { cancel(); return time.Now() },
		ModelsDevCatalog: func(refreshCtx context.Context) (*modelcatalog.Catalog, error) {
			<-refreshCtx.Done()
			close(finished)
			return nil, refreshCtx.Err()
		},
	}})
	if service != nil {
		defer service.Close()
	}
	if service != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("service=%v error=%v, want nil/context.Canceled", service, err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Open returned a startup cancellation before maintenance joined")
	}
}
