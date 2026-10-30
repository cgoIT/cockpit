package web

import (
	"crypto/ed25519"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"cockpit/internal/agents"
	"cockpit/internal/auth"
	"cockpit/internal/report"
	"cockpit/internal/sign"
)

func setupAgents(t *testing.T) (*Server, ed25519.PublicKey) {
	t.Helper()
	srv, _, _ := setup(t)
	priv, err := sign.LoadOrCreateKey(filepath.Join(t.TempDir(), "signing.key"))
	if err != nil {
		t.Fatal(err)
	}
	srv.SignKey, srv.Agents = priv, agents.Open(filepath.Join(t.TempDir(), "agents.json"))
	return srv, priv.Public().(ed25519.PublicKey)
}

func do(srv *Server, method, path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestAgentFlow(t *testing.T) {
	srv, pub := setupAgents(t)
	tok := ""
	// Anlegen über die Oberfläche: Token erscheint einmalig auf der Seite.
	w := do(srv, "POST", "/agents", url.Values{"id": {"server1"}}, nil)
	if w.Code != 200 || !strings.Contains(html.UnescapeString(w.Body.String()), sign.EncodePublicKey(pub)) {
		t.Fatalf("Anlegen: %d", w.Code)
	}
	a, _ := srv.Agents.Get("server1")
	for _, f := range strings.Fields(strings.ReplaceAll(w.Body.String(), `"`, " ")) {
		if len(f) == 64 && a.TokenHash != "" {
			tok = f
		}
	}
	if tok == "" {
		t.Fatal("Token nicht auf der Seite")
	}
	if w := do(srv, "GET", "/agents/server1", nil, nil); !strings.Contains(w.Body.String(), "Konfiguration, die der Agent erhält") {
		t.Error("Vorschau fehlt")
	}
	// Konfiguration speichern.
	form := url.Values{"count": {"2"}, "interval": {"10m"},
		"s0_id": {"kunde"}, "s0_name": {"Kunde"}, "s0_kind": {"contao"}, "s0_path": {"/var/www/kunde"}, "s0_no_nginx": {"1"}, "s0_ignore": {"contao/*:6.*\n"},
		"s1_id": {""}}
	if w := do(srv, "POST", "/agents/server1", form, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("Speichern: %d %s", w.Code, w.Body.String())
	}
	if w := do(srv, "POST", "/agents/server1", url.Values{"count": {"1"}, "s0_id": {"x"}, "s0_kind": {"contao"}}, nil); w.Code != 422 || !strings.Contains(w.Body.String(), "path fehlt") {
		t.Errorf("ungültig: %d", w.Code)
	}
	// Config-API: signiert, nur mit gültigem Token.
	if w := do(srv, "GET", "/api/v1/agent/config", nil, nil); w.Code != 401 {
		t.Errorf("ohne Token: %d", w.Code)
	}
	w = do(srv, "GET", "/api/v1/agent/config", nil, map[string]string{"Authorization": "Bearer " + tok})
	var env sign.Envelope
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &env) != nil {
		t.Fatalf("Config: %d", w.Code)
	}
	p, err := sign.Verify(pub, env, "server1", srv.Now(), srv.Now())
	if err != nil || !strings.Contains(string(p.Config), `"kunde"`) || !strings.Contains(string(p.Config), `contao/*:6.*`) {
		t.Fatalf("Verify: %v %s", err, p.Config)
	}
	if w := do(srv, "GET", "/agents/server1", nil, nil); !strings.Contains(w.Body.String(), "&#34;path&#34;: &#34;/var/www/kunde&#34;") {
		t.Error("Vorschau zeigt die gespeicherte Konfiguration nicht")
	}
	// Melden: nur für Sites der Konfiguration.
	rep := func(id string) report.Report {
		return report.Report{Schema: 1, CollectedAt: srv.Now(), Site: report.Site{ID: id, Kind: "contao"}}
	}
	if w := post(srv, tok, rep("kunde")); w.Code != http.StatusNoContent {
		t.Errorf("eigene Site: %d", w.Code)
	}
	if w := post(srv, tok, rep("fremd")); w.Code != http.StatusUnauthorized {
		t.Errorf("fremde Site: %d", w.Code)
	}
	// Übersicht kennt die Site auch ohne Meldung.
	if w := do(srv, "GET", "/", nil, nil); !strings.Contains(w.Body.String(), "kunde") {
		t.Error("Site fehlt in der Übersicht")
	}
	// Token erneuern: das alte ist sofort ungültig.
	do(srv, "POST", "/agents/server1/rotate", url.Values{}, nil)
	if w := post(srv, tok, rep("kunde")); w.Code != http.StatusUnauthorized {
		t.Errorf("altes Token: %d", w.Code)
	}
	// Löschen braucht die Bestätigung.
	if w := do(srv, "POST", "/agents/server1/delete", url.Values{}, nil); w.Code != 404 {
		t.Errorf("ohne Bestätigung: %d", w.Code)
	}
	if w := do(srv, "POST", "/agents/server1/delete", url.Values{"confirm": {"1"}}, nil); w.Code != http.StatusSeeOther {
		t.Errorf("löschen: %d", w.Code)
	}
}

func TestSameOriginGuardWithLogin(t *testing.T) {
	srv, _ := setupAgents(t)
	srv.Protect = func(next http.Handler) http.Handler { return next } // Login simuliert
	if w := do(srv, "POST", "/agents", url.Values{"id": {"x"}}, map[string]string{"Origin": "https://evil.example"}); w.Code != http.StatusForbidden {
		t.Errorf("mit Login: %d, erwartet 403", w.Code)
	}
}

func TestSaveRejectsBrokenForm(t *testing.T) {
	srv, _ := setupAgents(t)
	do(srv, "POST", "/agents", url.Values{"id": {"server1"}}, nil)
	ok := url.Values{"count": {"1"}, "s0_id": {"a"}, "s0_kind": {"php"}, "s0_path": {"/var/www/a"}}
	if w := do(srv, "POST", "/agents/server1", ok, nil); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code)
	}
	if w := do(srv, "POST", "/agents/server1", url.Values{"interval": {"5m"}}, nil); w.Code != http.StatusBadRequest {
		t.Errorf("ohne count: %d", w.Code)
	}
	if a, _ := srv.Agents.Get("server1"); len(a.Config.Sites) != 1 {
		t.Error("Konfiguration darf nicht geleert werden")
	}
	// Site mit eigenem Token (cockpit token) darf nicht zusätzlich einem Agenten gehören.
	if _, err := auth.Add(srv.TokensPath, "alt"); err != nil {
		t.Fatal(err)
	}
	dup := url.Values{"count": {"1"}, "s0_id": {"alt"}, "s0_kind": {"php"}, "s0_path": {"/var/www/alt"}}
	if w := do(srv, "POST", "/agents/server1", dup, nil); w.Code != 422 {
		t.Errorf("Kollision mit Einzel-Token: %d", w.Code)
	}
	if w := do(srv, "POST", "/agents/server1", url.Values{"count": {"1"}, "s0_id": {"b"}, "s0_kind": {"php"}, "s0_path": {"/x"}, "s0_url": {"ftp://x"}}, nil); w.Code != 422 {
		t.Errorf("url-Schema: %d", w.Code)
	}
}

func TestSameOriginGuard(t *testing.T) {
	srv, _ := setupAgents(t)
	for name, h := range map[string]map[string]string{
		"anderer Origin":     {"Origin": "https://evil.example"},
		"Sec-Fetch-Site":     {"Sec-Fetch-Site": "cross-site"},
		"gleiche Seite aber": nil,
	} {
		w := do(srv, "POST", "/agents", url.Values{"id": {"x" + strings.ReplaceAll(name, " ", "")[:3]}}, h)
		if h != nil && w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, erwartet 403", name, w.Code)
		}
		if h == nil && w.Code != 200 {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := do(srv, "POST", "/agents", url.Values{"id": {"ok"}}, map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}); w.Code != 200 {
		t.Errorf("gleicher Origin: %d", w.Code)
	}
}
