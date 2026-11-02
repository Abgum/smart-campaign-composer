package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"smart-campaign-composer/backend/internal/llm/provider"
	"smart-campaign-composer/backend/internal/mockprovider"
)

// config is read from the environment once at startup (see .env.example).
type config struct {
	DatabaseURL string     // DATABASE_URL
	LogLevel    slog.Level // LOG_LEVEL: debug | info | warn | error

	// api
	HTTPAddr        string          // HTTP_ADDR
	WebDir          string          // WEB_DIR: the built UI; empty serves the API only
	LLM             provider.Config // LLM_PROVIDER, ANTHROPIC_API_KEY, LLM_MODEL, LLM_EFFORT
	Router          provider.Config // LLM_ROUTER_MODEL, LLM_ROUTER_EFFORT; provider and key as LLM
	TurnTimeout     time.Duration   // TURN_TIMEOUT
	SSEWriteTimeout time.Duration   // SSE_WRITE_TIMEOUT
	TurnStaleAfter  time.Duration   // TURN_STALE_AFTER: the reaper ends turns whose heartbeat is older
	Location        *time.Location  // APP_TIMEZONE: the local days of the analytics

	// worker
	ProviderURL       string // PROVIDER_URL
	SMSRate           int    // SMS_RATE_PER_SECOND
	EmailRate         int    // EMAIL_RATE_PER_SECOND
	WorkerName        string // WORKER_NAME; empty means host:pid
	WorkerMetricsAddr string // WORKER_METRICS_ADDR

	// mockprovider
	MockAddr   string                         // MOCKPROVIDER_ADDR
	MockLog    string                         // MOCKPROVIDER_LOG: the acceptance log
	MockFaults map[mockprovider.Fault]float64 // MOCKPROVIDER_FAULTS
	MockDelay  time.Duration                  // MOCKPROVIDER_DELAY: response delay when the delay fault is selected
	MockSeed   uint64                         // MOCKPROVIDER_SEED

	// seed
	SeedUsers  int    // SEED_USERS
	SeedRandom uint64 // SEED_RANDOM
}

// loadConfig reads the configuration. A value that does not parse is an
// error naming its variable; nothing falls back to a default silently.
func loadConfig(getenv func(string) string) (config, error) {
	p := &envParser{getenv: getenv}
	cfg := config{
		DatabaseURL: p.str("DATABASE_URL", ""),
		LogLevel:    p.level("LOG_LEVEL", slog.LevelInfo),

		HTTPAddr: p.str("HTTP_ADDR", ":8080"),
		WebDir:   p.str("WEB_DIR", ""),
		LLM: provider.Config{Provider: p.str("LLM_PROVIDER", "auto"), APIKey: p.str("ANTHROPIC_API_KEY", ""),
			Model: p.str("LLM_MODEL", "claude-opus-5-5"), Effort: p.str("LLM_EFFORT", "medium")},
		TurnTimeout:     p.duration("TURN_TIMEOUT", 120*time.Second),
		SSEWriteTimeout: p.duration("SSE_WRITE_TIMEOUT", 10*time.Second),
		TurnStaleAfter:  p.duration("TURN_STALE_AFTER", 30*time.Second),
		Location:        p.location("APP_TIMEZONE", "Europe/Istanbul"),

		ProviderURL:       p.str("PROVIDER_URL", "http://localhost:8090"),
		SMSRate:           p.positive("SMS_RATE_PER_SECOND", 100),
		EmailRate:         p.positive("EMAIL_RATE_PER_SECOND", 100),
		WorkerName:        p.str("WORKER_NAME", ""),
		WorkerMetricsAddr: p.str("WORKER_METRICS_ADDR", ":9091"),

		MockAddr:   p.str("MOCKPROVIDER_ADDR", ":8090"),
		MockLog:    p.str("MOCKPROVIDER_LOG", "mockprovider.jsonl"),
		MockFaults: p.faults("MOCKPROVIDER_FAULTS"),
		MockDelay:  p.duration("MOCKPROVIDER_DELAY", 200*time.Millisecond),
		MockSeed:   p.uint("MOCKPROVIDER_SEED", 1),

		SeedUsers:  p.positive("SEED_USERS", 5000),
		SeedRandom: p.uint("SEED_RANDOM", 42),
	}
	// The router is Claude too (a cheaper effort by default); in mock mode
	// the demo model plays it.
	cfg.Router = provider.Config{Provider: cfg.LLM.Provider, APIKey: cfg.LLM.APIKey,
		Model: p.str("LLM_ROUTER_MODEL", cfg.LLM.Model), Effort: p.str("LLM_ROUTER_EFFORT", "low")}
	return cfg, errors.Join(p.errs...)
}

// require checks what a mode cannot start without.
func (c config) require(mode string) error {
	switch mode {
	case "api", "worker", "migrate", "seed", "history":
		if c.DatabaseURL == "" {
			return fmt.Errorf("%s needs DATABASE_URL", mode)
		}
	}
	return nil
}

// envParser reads typed variables and collects what does not parse.
type envParser struct {
	getenv func(string) string
	errs   []error
}

func (p *envParser) fail(name, val, want string) {
	p.errs = append(p.errs, fmt.Errorf("%s=%q: want %s", name, val, want))
}

func (p *envParser) str(name, def string) string {
	if v := strings.TrimSpace(p.getenv(name)); v != "" {
		return v
	}
	return def
}

func (p *envParser) duration(name string, def time.Duration) time.Duration {
	v := p.str(name, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.fail(name, v, "a positive duration such as 30s")
		return def
	}
	return d
}

func (p *envParser) positive(name string, def int) int {
	v := p.str(name, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		p.fail(name, v, "a positive integer")
		return def
	}
	return n
}

func (p *envParser) uint(name string, def uint64) uint64 {
	v := p.str(name, "")
	if v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		p.fail(name, v, "a non-negative integer")
		return def
	}
	return n
}

func (p *envParser) location(name, def string) *time.Location {
	v := p.str(name, def)
	loc, err := time.LoadLocation(v)
	if err != nil {
		p.fail(name, v, "an IANA time zone such as Europe/Istanbul")
		loc, _ = time.LoadLocation(def)
	}
	return loc
}

func (p *envParser) level(name string, def slog.Level) slog.Level {
	v := p.str(name, "")
	if v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail(name, v, "debug, info, warn or error")
		return def
	}
	return l
}

func (p *envParser) faults(name string) map[mockprovider.Fault]float64 {
	rates, err := mockprovider.ParseRates(p.getenv(name))
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: %w", name, err))
		return nil
	}
	return rates
}
