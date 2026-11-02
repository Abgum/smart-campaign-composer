package provider_test

import (
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/llm/provider"
)

func TestProviderAuto_NoKey_UsesMockWithReason(t *testing.T) {
	c, err := provider.Select(provider.Config{Provider: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	info := c.Info()
	if info.Provider != "mock" {
		t.Fatalf("provider = %q, want mock", info.Provider)
	}
	if !strings.Contains(info.Reason, "ANTHROPIC_API_KEY") {
		t.Fatalf("reason = %q, must say why mock was chosen", info.Reason)
	}
}

func TestProviderAuto_WithKey_UsesAnthropic(t *testing.T) {
	c, err := provider.Select(provider.Config{Provider: "auto", APIKey: "sk-test", Model: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if info := c.Info(); info.Provider != "anthropic" || info.Model != "claude-opus-5-5" || info.Reason != "" {
		t.Fatalf("info = %+v", info)
	}
}

func TestProviderAnthropic_NoKey_FailsStartup(t *testing.T) {
	if _, err := provider.Select(provider.Config{Provider: "anthropic"}); err == nil {
		t.Fatal("LLM_PROVIDER=anthropic without a key must fail instead of falling back to mock")
	}
}

func TestProviderMock_Explicit(t *testing.T) {
	c, err := provider.Select(provider.Config{Provider: "mock", APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	if info := c.Info(); info.Provider != "mock" || !strings.Contains(info.Reason, "LLM_PROVIDER=mock") {
		t.Fatalf("info = %+v", info)
	}
}

func TestProvider_Unknown(t *testing.T) {
	if _, err := provider.Select(provider.Config{Provider: "openai"}); err == nil {
		t.Fatal("unknown provider must fail")
	}
}
