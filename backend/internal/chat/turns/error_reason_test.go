package turns

import (
	"testing"

	"smart-campaign-composer/backend/internal/i18n"
)

func TestBadRequestReason_IdentifiesInvalidAPIRequest(t *testing.T) {
	got := reason("llm_bad_request", voice{name: "Claude"})
	if got != "uygulamanın model API isteği geçersiz" {
		t.Fatalf("Turkish bad-request reason = %q", got)
	}
	if english := i18n.Translate(i18n.EN, got); english != "the app sent an invalid request to the model API" {
		t.Fatalf("English bad-request reason = %q", english)
	}
}
