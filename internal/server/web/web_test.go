package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h := Handler()
	for path, want := range map[string]string{
		"/":                 "<title>nostromo</title>",
		"/app.js":           "use strict",
		"/app.css":          "--accent",
		"/vendor/xterm.js":  "Terminal",
		"/vendor/xterm.css": ".xterm",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d", path, w.Code)
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("%s: missing CSP", path)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/vendor/addon-fit.js", nil))
	if !strings.Contains(w.Header().Get("Cache-Control"), "max-age") {
		t.Fatal("vendor files are cacheable")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))
	if w.Code != 404 {
		t.Fatalf("missing file: %d", w.Code)
	}
}
