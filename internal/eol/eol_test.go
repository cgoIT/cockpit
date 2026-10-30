package eol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func server(t *testing.T, fail *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *fail {
			http.Error(w, "kaputt", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"result":{"releases":[{"name":"1.0","eolFrom":"2026-12-31"},{"name":"2.0","eolFrom":null}]}}`))
	}))
}

func TestRefreshLookupAndCache(t *testing.T) {
	fail := false
	srv := server(t, &fail)
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "eol.json")
	p := &Provider{CacheFile: cache, BaseURL: srv.URL + "/"}
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := p.EOL("php", "1.0.7"); !ok || got.Year() != 2026 || got.Month() != 12 || got.Day() != 31 {
		t.Fatalf("php 1.0.7: %v %v", got, ok)
	}
	if _, ok := p.EOL("php", "2.0.1"); ok { // eolFrom null = kein Ende
		t.Error("null-Datum darf kein Ende ergeben")
	}
	if _, ok := p.EOL("php", "9.9"); ok {
		t.Error("unbekannte Version")
	}

	fail = true // Ausfall: alte Daten bleiben, Fehler wird gemeldet
	if err := p.Refresh(context.Background()); err == nil {
		t.Error("Fehler erwartet")
	}
	if _, ok := p.EOL("php", "1.0"); !ok {
		t.Error("alte Daten verloren")
	}

	q := &Provider{CacheFile: cache} // Neustart: Zwischenspeicher
	if err := q.Load(); err != nil {
		t.Fatal(err)
	}
	if got, ok := q.EOL("nginx", "1.0"); !ok || got.Before(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Cache: %v %v", got, ok)
	}
}

func TestNilProviderAndMinor(t *testing.T) {
	var p *Provider
	if _, ok := p.EOL("php", "8.5"); ok {
		t.Error("nil-Provider")
	}
	if Minor("5.7.15") != "5.7" || Minor("8") != "" {
		t.Error("Minor")
	}
}
