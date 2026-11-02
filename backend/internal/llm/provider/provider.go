// Package provider selects the llm.Client from configuration.
package provider

import (
	"errors"
	"fmt"
	"time"

	"smart-campaign-composer/backend/internal/llm"
	"smart-campaign-composer/backend/internal/llm/anthropic"
	"smart-campaign-composer/backend/internal/llm/mock"
)

// Config mirrors the LLM_* environment variables.
type Config struct {
	Provider string // auto | anthropic | mock
	APIKey   string
	Model    string
	Effort   string
}

// Select returns the configured client. "anthropic" without a key is a
// startup error; only "auto" may fall back to mock, and the fallback reason
// is carried in Info so the UI can show it. There is no runtime fallback.
func Select(cfg Config) (llm.Client, error) {
	switch cfg.Provider {
	case "anthropic":
		if cfg.APIKey == "" {
			return nil, errors.New("LLM_PROVIDER=anthropic but ANTHROPIC_API_KEY is empty")
		}
		return newAnthropic(cfg)
	case "mock":
		return mock.NewDemo("LLM_PROVIDER=mock"), nil
	case "auto", "":
		if cfg.APIKey == "" {
			return mock.NewDemo("ANTHROPIC_API_KEY tanımlı değil; LLM_PROVIDER=auto mock'a geçti"), nil
		}
		return newAnthropic(cfg)
	}
	return nil, fmt.Errorf("unknown LLM_PROVIDER %q (auto | anthropic | mock)", cfg.Provider)
}

func newAnthropic(cfg Config) (llm.Client, error) {
	return anthropic.New(anthropic.Config{
		APIKey:     cfg.APIKey,
		Model:      cfg.Model,
		Effort:     cfg.Effort,
		MaxRetries: 2,
		Timeout:    120 * time.Second,
	})
}
