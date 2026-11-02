package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"smart-campaign-composer/backend/internal/i18n"
)

// Answers speak the language the request asks for (Accept-Language or
// ?lang=, Turkish by default). Handlers write their messages as they are;
// an error answer (a 4xx or 5xx JSON body) is held back and translated on
// its way out, every "message" in it, the issues of a rejected draft
// included. The event stream does the same for the events that carry
// messages. Error codes stay the same in every language.

// langWriter holds back an error answer to translate it.
type langWriter struct {
	http.ResponseWriter
	lang    i18n.Lang
	wrote   bool          // the status line is out
	status  int           // of a held-back answer
	pending *bytes.Buffer // the held-back body
}

func (w *langWriter) WriteHeader(code int) {
	if w.wrote || w.pending != nil {
		return
	}
	if code >= http.StatusBadRequest {
		w.status, w.pending = code, &bytes.Buffer{}
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *langWriter) Write(b []byte) (int, error) {
	if w.pending != nil {
		return w.pending.Write(b)
	}
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Flush lets an event stream through at once.
func (w *langWriter) Flush() {
	if w.pending == nil {
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
}

// Unwrap gives http.ResponseController the connection underneath.
func (w *langWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish sends a held-back error answer in the request's language.
func (w *langWriter) finish() {
	if w.pending == nil {
		return
	}
	body := w.pending.Bytes()
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		body = localizeJSON(w.lang, body)
	}
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(body)
}

// langOf finds the answer language of w (Turkish if it has none).
func langOf(w http.ResponseWriter) i18n.Lang {
	for {
		if lw, ok := w.(*langWriter); ok {
			return lw.lang
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return i18n.TR
		}
		w = u.Unwrap()
	}
}

// localizeJSON translates every "message" string in a JSON document, at any
// depth; anything else, numbers included, is kept as it was.
func localizeJSON(lang i18n.Lang, b []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return b
	}
	out, err := json.Marshal(localize(lang, v))
	if err != nil {
		return b
	}
	if bytes.HasSuffix(b, []byte("\n")) {
		out = append(out, '\n')
	}
	return out
}

func localize(lang i18n.Lang, v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok && k == "message" {
				x[k] = i18n.Translate(lang, s)
				continue
			}
			x[k] = localize(lang, val)
		}
	case []any:
		for i := range x {
			x[i] = localize(lang, x[i])
		}
	}
	return v
}
