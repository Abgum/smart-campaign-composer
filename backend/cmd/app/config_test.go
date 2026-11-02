package main

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/mockprovider"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"DATABASE_URL": "postgres://db"}))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"HTTPAddr", cfg.HTTPAddr, ":8080"},
		{"LLM.Provider", cfg.LLM.Provider, "auto"},
		{"LLM.Model", cfg.LLM.Model, "claude-opus-5-5"},
		{"LLM.Effort", cfg.LLM.Effort, "medium"},
		{"Router.Model", cfg.Router.Model, "claude-opus-5-5"},
		{"Router.Effort", cfg.Router.Effort, "low"},
		{"TurnTimeout", cfg.TurnTimeout, 120 * time.Second},
		{"SSEWriteTimeout", cfg.SSEWriteTimeout, 10 * time.Second},
		{"TurnStaleAfter", cfg.TurnStaleAfter, 30 * time.Second},
		{"ProviderURL", cfg.ProviderURL, "http://localhost:8090"},
		{"SMSRate", cfg.SMSRate, 100},
		{"EmailRate", cfg.EmailRate, 100},
		{"WorkerMetricsAddr", cfg.WorkerMetricsAddr, ":9091"},
		{"MockAddr", cfg.MockAddr, ":8090"},
		{"MockLog", cfg.MockLog, "mockprovider.jsonl"},
		{"MockFaults", len(cfg.MockFaults), 0},
		{"MockDelay", cfg.MockDelay, 200 * time.Millisecond},
		{"SeedUsers", cfg.SeedUsers, 5000},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
		{"Location", cfg.Location.String(), "Europe/Istanbul"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadConfig_Overrides(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"DATABASE_URL": "postgres://db", "HTTP_ADDR": ":9000", "WEB_DIR": "/web",
		"LLM_PROVIDER": "anthropic", "ANTHROPIC_API_KEY": "sk-test", "LLM_MODEL": "claude-sonnet-5-5", "LLM_EFFORT": "high",
		"LLM_ROUTER_MODEL": "claude-haiku-5-5", "LLM_ROUTER_EFFORT": "medium",
		"TURN_TIMEOUT": "90s", "SSE_WRITE_TIMEOUT": "5s", "SMS_RATE_PER_SECOND": "20",
		"MOCKPROVIDER_FAULTS": "reject_429=0.1,accept_then_drop=0.05,delay=0.02", "MOCKPROVIDER_DELAY": "350ms", "MOCKPROVIDER_SEED": "7", "LOG_LEVEL": "debug",
		"APP_TIMEZONE": "UTC",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9000" || cfg.WebDir != "/web" || cfg.LLM.Provider != "anthropic" || cfg.LLM.APIKey != "sk-test" ||
		cfg.LLM.Model != "claude-sonnet-5-5" || cfg.LLM.Effort != "high" || cfg.Router.APIKey != "sk-test" ||
		cfg.Router.Model != "claude-haiku-5-5" || cfg.Router.Effort != "medium" || cfg.TurnTimeout != 90*time.Second ||
		cfg.SSEWriteTimeout != 5*time.Second || cfg.SMSRate != 20 || cfg.EmailRate != 100 || cfg.MockSeed != 7 ||
		cfg.MockFaults[mockprovider.Reject429] != 0.1 || cfg.MockFaults[mockprovider.AcceptThenDrop] != 0.05 ||
		cfg.MockFaults[mockprovider.Delay] != 0.02 || cfg.MockDelay != 350*time.Millisecond ||
		cfg.LogLevel != slog.LevelDebug || cfg.Location.String() != "UTC" {
		t.Fatalf("config = %+v", cfg)
	}
}

// A bad value fails at startup and names its variable; nothing falls back
// to a default silently.
func TestLoadConfig_RejectsBadValues(t *testing.T) {
	for name, val := range map[string]string{
		"TURN_TIMEOUT":        "soon",
		"SSE_WRITE_TIMEOUT":   "0s",
		"SMS_RATE_PER_SECOND": "0",
		"SEED_USERS":          "-5",
		"MOCKPROVIDER_FAULTS": "reject_429=0.7,fail_503=0.7",
		"MOCKPROVIDER_SEED":   "x",
		"MOCKPROVIDER_DELAY":  "0s",
		"LOG_LEVEL":           "loud",
		"APP_TIMEZONE":        "Mars/Olympus",
	} {
		_, err := loadConfig(env(map[string]string{"DATABASE_URL": "postgres://db", name: val}))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: err = %v; want an error naming %s", name, val, err, name)
		}
	}
}

func TestRequire_DatabaseURLWhereNeeded(t *testing.T) {
	cfg, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"api", "worker", "migrate", "seed", "history"} {
		if err := cfg.require(mode); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
			t.Errorf("%s without DATABASE_URL: err = %v", mode, err)
		}
	}
	if err := cfg.require("mockprovider"); err != nil {
		t.Errorf("mockprovider needs no database: %v", err)
	}
}
