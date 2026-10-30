package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sitewatch/internal/auth"
	"sitewatch/internal/report"
	"sitewatch/internal/status"
	"sitewatch/internal/store"
)

func setup(t *testing.T) (*Server, string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "data", "sitewatch.db"), store.DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	tokens := filepath.Join(dir, "tokens.json")
	tok, err := auth.Add(tokens, "kunde-c")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 19, 0, 0, 0, time.UTC)
	srv := New(st, tokens, status.DefaultConfig())
	srv.Now = func() time.Time { return now }
	return srv, tok, now
}

func post(srv *Server, token string, rep report.Report) *httptest.ResponseRecorder {
	body, _ := json.Marshal(rep)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reports", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestIngestAndOverview(t *testing.T) {
	srv, tok, now := setup(t)
	tls, three := now.Add(60*24*time.Hour), 3
	rep := report.Report{
		Schema: 1, CollectedAt: now,
		Site: report.Site{ID: "kunde-c", Name: "Kunde C", Kind: "contao", PHP: "8.3.12", Contao: "5.3.9", TLSExpires: &tls, BackupCount: &three,
			Advisories: []report.Advisory{{Package: "symfony/http-kernel", Title: "Beispiel"}}},
	}
	if w := post(srv, tok, rep); w.Code != http.StatusNoContent {
		t.Fatalf("Ingest: %d %s", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Kunde C") || !strings.Contains(w.Body.String(), "Kritisch") {
		t.Fatalf("Übersicht unerwartet: %d", w.Code)
	}
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sites/kunde-c", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Sicherheitshinweis") || !strings.Contains(w.Body.String(), "TLS läuft ab") || !strings.Contains(w.Body.String(), ">3<") ||
		!strings.Contains(w.Body.String(), tls.Local().Format("02.01.2006")) {
		t.Fatalf("Detail unerwartet: %d", w.Code)
	}
}

func TestIngestRejects(t *testing.T) {
	srv, tok, now := setup(t)
	ok := report.Report{Schema: 1, CollectedAt: now, Site: report.Site{ID: "kunde-c", Kind: "contao"}}

	if w := post(srv, "", ok); w.Code != http.StatusUnauthorized {
		t.Errorf("ohne Token: %d", w.Code)
	}
	if w := post(srv, "falsch", ok); w.Code != http.StatusUnauthorized {
		t.Errorf("falsches Token: %d", w.Code)
	}
	other := ok
	other.Site.ID = "kunde-x" // Token gehört nicht zu dieser Site
	if w := post(srv, tok, other); w.Code != http.StatusUnauthorized {
		t.Errorf("fremde Site: %d", w.Code)
	}
	bad := ok
	bad.Schema = 9
	if w := post(srv, tok, bad); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("ungültiges Schema: %d", w.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reports", strings.NewReader("{kaputt"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("kaputtes JSON: %d", w.Code)
	}
}

func TestSilentSiteIsCritical(t *testing.T) {
	srv, _, _ := setup(t) // kunde-c ist registriert, hat aber nie gemeldet
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(w.Body.String(), "Noch keine Meldung") {
		t.Errorf("stille Site fehlt in der Übersicht")
	}
}

func TestProtectGuardsUIButNotAgentAPI(t *testing.T) {
	srv, _, _ := setup(t)
	srv.Protect = func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	}
	h := srv.Handler()
	for _, path := range []string{"/", "/sites/kunde-c"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: Code %d, erwartet 403", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz: Code %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/foo", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("/foo: Code %d, erwartet 403", rec.Code)
	}
	// Die Agent-API darf nicht am Login hängen: ohne Token 401 statt 403.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/reports", strings.NewReader("{}")))
	if rec.Code == http.StatusForbidden {
		t.Error("Agent-API läuft hinter Protect")
	}
}

func TestDetailUsesCollectedAtAndIgnoresRuleHits(t *testing.T) {
	srv, tok, now := setup(t)
	rep := report.Report{Schema: 1, CollectedAt: now.Add(-3 * time.Hour), Site: report.Site{ID: "kunde-c", Name: "Kunde C", Kind: "contao",
		Outdated: []report.Package{{Name: "contao/news-bundle", Installed: "5.7.15", Latest: "6.0.4", Status: "update-possible", Ignored: true}}}}
	if w := post(srv, tok, rep); w.Code != http.StatusNoContent {
		t.Fatalf("Ingest: %d %s", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sites/kunde-c", nil))
	body := w.Body.String()
	if !strings.Contains(body, rep.CollectedAt.Local().Format("02.01. 15:04")) || !strings.Contains(body, "ausgeschlossen") || strings.Contains(body, "Paket(e) mit verfügbarem Update") {
		t.Errorf("Detail unerwartet: %s", body)
	}
}

func TestLogoutButtonOnlyWithLogin(t *testing.T) {
	srv, _, _ := setup(t)
	get := func() (string, string) {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Body.String(), rec.Header().Get("Content-Security-Policy")
	}
	if body, _ := get(); strings.Contains(body, `action="/logout"`) {
		t.Error("Abmelden ohne Login sichtbar")
	}
	srv.Protect = func(next http.Handler) http.Handler { return next }
	body, csp := get()
	if !strings.Contains(body, `action="/logout"`) {
		t.Error("Abmelden fehlt")
	}
	if !strings.Contains(csp, "form-action 'self'") {
		t.Errorf("CSP: %s", csp)
	}
}

func TestDetailShowsCronAndMigrations(t *testing.T) {
	srv, tok, now := setup(t)
	zero, cron := 0, now.Add(-2*time.Minute)
	rep := report.Report{Schema: 1, CollectedAt: now, Site: report.Site{ID: "kunde-c", Name: "Kunde C", Kind: "contao",
		PendingMigrations: &zero, CronLastRun: &cron}}
	if w := post(srv, tok, rep); w.Code != http.StatusNoContent {
		t.Fatalf("Ingest: %d %s", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sites/kunde-c", nil))
	body := w.Body.String()
	if !strings.Contains(body, `Offene Migrationen</dt><dd class="mono">0</dd>`) || !strings.Contains(body, cron.Local().Format("02.01. 15:04")) {
		t.Errorf("Cron/Migrationen fehlen: %s", body)
	}
}

func TestDetailDaySnapshot(t *testing.T) {
	srv, tok, now := setup(t)
	if w := post(srv, tok, report.Report{Schema: 1, CollectedAt: now, Site: report.Site{ID: "kunde-c", Kind: "contao"}}); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	get := func(q string) (int, string) {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sites/kunde-c"+q, nil))
		return w.Code, w.Body.String()
	}
	if c, b := get(""); c != 200 || strings.Contains(b, "Tagesstand vom") || !strings.Contains(b, `value="2026-10-27">27.10.2026`) {
		t.Errorf("aktueller Stand: %d, Liste der Tage fehlt oder Banner zu viel", c)
	}
	if c, b := get("?day=2026-10-27"); c != 200 || !strings.Contains(b, "Tagesstand vom 27.10.2026") {
		t.Errorf("Tagesstand: %d", c)
	}
	for _, q := range []string{"?day=2026-10-01", "?day=quatsch"} {
		if c, _ := get(q); c != 404 {
			t.Errorf("%s: %d, erwartet 404", q, c)
		}
	}
}
