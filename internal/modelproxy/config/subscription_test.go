package config

import (
	"testing"
	"time"

	"harness/internal/configmeta"
)

func TestSubscriptionPollIntervalPrecedenceAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, file, env, flag string
		want                  time.Duration
		source                configmeta.SourceKind
	}{
		{name: "default", want: 5 * time.Minute, source: configmeta.SourceDefault},
		{name: "file", file: `"2m"`, want: 2 * time.Minute, source: configmeta.SourceFile},
		{name: "integer seconds", file: `120`, want: 2 * time.Minute, source: configmeta.SourceFile},
		{name: "minimum", flag: "1m", want: time.Minute, source: configmeta.SourceFlag},
		{name: "file disabled", file: `0`, want: 0, source: configmeta.SourceFile},
		{name: "environment", file: `"2m"`, env: "3m", want: 3 * time.Minute, source: configmeta.SourceEnvironment},
		{name: "environment disabled", file: `"2m"`, env: "0", want: 0, source: configmeta.SourceEnvironment},
		{name: "flag", file: `"2m"`, env: "3m", flag: "4m", want: 4 * time.Minute, source: configmeta.SourceFlag},
		{name: "flag disabled", file: `"2m"`, env: "3m", flag: "0", want: 0, source: configmeta.SourceFlag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := LoadOptions{Getenv: getenv(map[string]string{subscriptionPollEnvironment: tc.env}), DeriveInstanceID: fixedInstance("test")}
			if tc.file != "" {
				opts.Path = writeConfig(t, t.TempDir(), `{"subscription_poll_interval":`+tc.file+`}`)
				opts.PathExplicit = true
			}
			if tc.flag != "" {
				opts.Flags = parseFlags(t, "-subscription-poll-interval", tc.flag)
			}
			got := load(t, opts)
			if got.Config.SubscriptionPollInterval.Duration != tc.want || got.Sources["subscription_poll_interval"].Kind != tc.source {
				t.Fatalf("config %+v source %+v", got.Config, got.Sources["subscription_poll_interval"])
			}
			if tc.source == configmeta.SourceEnvironment && got.Sources["subscription_poll_interval"].Name != subscriptionPollEnvironment {
				t.Fatal(got.Sources)
			}
		})
	}
	for _, value := range []string{"-1s", "bad", "", "1ns", "59s"} {
		if _, err := Load(LoadOptions{Getenv: getenv(nil), Flags: parseFlags(t, "-subscription-poll-interval="+value)}); err == nil {
			t.Errorf("accepted flag %q", value)
		}
	}
	for _, value := range []string{"-1s", "bad", "1ms", "59s"} {
		if _, err := Load(LoadOptions{Getenv: getenv(map[string]string{subscriptionPollEnvironment: value})}); err == nil {
			t.Errorf("accepted environment %q", value)
		}
	}
	for _, value := range []string{`-1`, `"-1s"`, `1`, `"59s"`, `9223372036854775807`} {
		path := writeConfig(t, t.TempDir(), `{"subscription_poll_interval":`+value+`}`)
		if _, err := Load(LoadOptions{Path: path, PathExplicit: true, Getenv: getenv(nil)}); err == nil {
			t.Errorf("accepted file %s", value)
		}
	}
}
