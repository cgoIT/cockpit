package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Manifest und Symbol müssen auch hinter dem Login erreichbar sein; die Fußzeile zeigt die Version.
func TestManifestOhneLoginUndFusszeile(t *testing.T) {
	srv, _ := setupAgents(t)
	srv.Protect = func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "login", http.StatusUnauthorized) })
	}
	srv.Version = "1.2.3"
	h := srv.Handler()
	for _, p := range []string{"/manifest.webmanifest", "/icon.svg", "/icon-192.png", "/icon-512.png", "/apple-touch-icon.png"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if !json.Valid([]byte(manifestJSON)) {
		t.Error("Manifest ist kein gültiges JSON")
	}
	srv.Protect = nil
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if b := w.Body.String(); !strings.Contains(b, "Sitewatch 1.2.3") || !strings.Contains(b, `rel="manifest"`) {
		t.Error("Fußzeile oder Manifest-Link fehlt")
	}
	if v := srv.versionText(); !strings.HasPrefix(v, "1.2.3") {
		t.Errorf("versionText = %q", v)
	}
}
