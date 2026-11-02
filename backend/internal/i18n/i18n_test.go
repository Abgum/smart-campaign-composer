package i18n

import (
	"net/http/httptest"
	"testing"
)

func TestFromRequest(t *testing.T) {
	for header, want := range map[string]Lang{
		"":                        TR,
		"tr":                      TR,
		"tr-TR,tr;q=0.9,en;q=0.8": TR,
		"en":                      EN,
		"en-US,en;q=0.9":          EN,
		"de-DE":                   TR, // neither: the app's own language
	} {
		r := httptest.NewRequest("GET", "/api/x", nil)
		if header != "" {
			r.Header.Set("Accept-Language", header)
		}
		if got := FromRequest(r); got != want {
			t.Errorf("Accept-Language %q -> %s, want %s", header, got, want)
		}
	}
	r := httptest.NewRequest("GET", "/api/x?lang=en", nil)
	r.Header.Set("Accept-Language", "tr")
	if got := FromRequest(r); got != EN {
		t.Errorf("?lang=en -> %s, want en", got)
	}
}

func TestTranslate(t *testing.T) {
	cases := []struct {
		lang     Lang
		in, want string
	}{
		{EN, "kampanya bulunamadı", "campaign not found"},
		{TR, "kampanya bulunamadı", "kampanya bulunamadı"},
		{EN, "mesaj en fazla 4000 karakter olabilir", "the message can be at most 4000 characters"},
		{EN, `"{{ad}}" desteklenmiyor; izinli değişkenler: {{first_name}}, {{last_name}}, {{city}}`,
			`"{{ad}}" is not supported; allowed variables: {{first_name}}, {{last_name}}, {{city}}`},
		{EN, "Mock model geçici olarak yanıt vermiyor", "Mock model is temporarily not responding"},
		// written in English in the code, read in Turkish by default
		{TR, `analytics: bad filter: range "nope" (today, day, 7d, 30d, 90d, all)`, `geçersiz filtre: range "nope" (today, day, 7d, 30d, 90d, all)`},
		{EN, `analytics: bad filter: range "nope" (today, day, 7d, 30d, 90d, all)`, `invalid filter: range "nope" (today, day, 7d, 30d, 90d, all)`},
		// a text with no entry stays as it is
		{EN, "bilinmeyen bir cümle", "bilinmeyen bir cümle"},
	}
	for _, c := range cases {
		if got := Translate(c.lang, c.in); got != c.want {
			t.Errorf("Translate(%s, %q) = %q, want %q", c.lang, c.in, got, c.want)
		}
	}
}
