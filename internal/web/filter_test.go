package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sitewatch/internal/report"
)

func TestOverviewFilter(t *testing.T) {
	srv, tok, now := setup(t)
	if w := post(srv, tok, report.Report{Schema: 1, CollectedAt: now, Site: report.Site{ID: "kunde-c", Name: "Kunde C", Kind: "contao"}}); w.Code != http.StatusNoContent {
		t.Fatalf("Ingest: %d %s", w.Code, w.Body.String())
	}
	get := func(q string) string {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?"+q, nil))
		return w.Body.String()
	}
	for q, want := range map[string]bool{
		"": true, "q=kunde": true, "q=KUNDE": true, "q=gibtesnicht": false,
		"kind=contao": true, "kind=php": false, "kind=unsinn": true, // unbekannte Werte werden ignoriert
		"status=crit": false, "status=problems": false, // die frische Meldung ist ok
	} {
		got := strings.Contains(get(q), `href="/sites/kunde-c"`)
		if got != want {
			t.Errorf("%q: sichtbar=%v, erwartet %v", q, got, want)
		}
	}
	if !strings.Contains(get("q=gibtesnicht"), "Keine Site passt zum Filter.") || !strings.Contains(get("q=a"), "Zurücksetzen") {
		t.Error("Hinweis oder Zurücksetzen fehlt")
	}
}
