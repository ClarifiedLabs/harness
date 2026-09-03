package otel

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	minExportTimeout       = time.Second
	maxExportTimeout       = 30 * time.Second
	DefaultExportTimeout   = 5 * time.Second
	PeriodicExportInterval = 30 * time.Second
	ShutdownExportTimeout  = 2 * time.Second
)

// Config is the exporter configuration derived from config.OTelConfig and runtime resource.
type Config struct {
	Enabled            bool
	Endpoint           string
	Protocol           string
	Timeout            time.Duration
	ServiceName        string
	Hostname           string
	Headers            map[string]string
	ResourceAttributes map[string]string
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Endpoint) == "" {
		return fmt.Errorf("otel endpoint is required when otel is enabled")
	}
	if _, err := normalizeEndpoint(c.Endpoint); err != nil {
		return err
	}
	if c.Protocol != "" && strings.ToLower(strings.TrimSpace(c.Protocol)) != "http/json" {
		return fmt.Errorf("otel protocol must be http/json")
	}
	if c.Timeout != 0 && (c.Timeout < minExportTimeout || c.Timeout > maxExportTimeout) {
		return fmt.Errorf("otel timeout must be between 1s and 30s")
	}
	return nil
}

// NormalizedEndpoint returns the endpoint normalized to include /v1/metrics.
func (c Config) NormalizedEndpoint() (string, error) {
	return normalizeEndpoint(c.Endpoint)
}

func normalizeEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("otel endpoint must not be empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("otel endpoint must be an absolute http(s) URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("otel endpoint must be an absolute http(s) URL")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("otel endpoint must not contain user info")
	}
	if parsed.Fragment != "" {
		return "", fmt.Errorf("otel endpoint must not contain a fragment")
	}

	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if !strings.HasSuffix(path, "/v1/metrics") {
		path += "/v1/metrics"
	}
	decodedPath, err := url.PathUnescape(path)
	if err != nil {
		return "", fmt.Errorf("otel endpoint has an invalid path: %w", err)
	}
	parsed.Path = decodedPath
	parsed.RawPath = path
	return parsed.String(), nil
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
