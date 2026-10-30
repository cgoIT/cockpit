package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sitewatch/internal/agentcfg"
	"sitewatch/internal/sign"
)

func TestLoadRemote(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	good := agentcfg.Remote{Sites: []agentcfg.Site{{ID: "kunde", Kind: "contao", Path: "/var/www/kunde"}}}
	evil := agentcfg.Remote{Sites: []agentcfg.Site{{ID: "kunde", Kind: "contao", Path: "/etc"}}}
	var serve any = good
	var issued = now
	up := true
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "nein", 503)
			return
		}
		env, _ := sign.Config(priv, "server1", issued, serve)
		_ = json.NewEncoder(w).Encode(env)
	}))
	defer ts.Close()
	bs := agentcfg.Bootstrap{Sitewatch: ts.URL, Agent: "server1", Token: "tok", PublicKey: pub, State: t.TempDir(), Roots: []string{"/var/www"}}
	ctx := context.Background()
	c := &http.Client{Timeout: time.Second}

	if r, err := loadRemote(ctx, c, bs, now); err != nil || len(r.Sites) != 1 {
		t.Fatalf("frisch: %v", err)
	}
	if _, err := readCacheFor(bs); err != nil {
		t.Fatal("Zwischenspeicher fehlt")
	}
	// Sitewatch nicht erreichbar: der Zwischenspeicher gilt.
	up = false
	if r, err := loadRemote(ctx, c, bs, now.Add(24*time.Hour)); err != nil || len(r.Sites) != 1 {
		t.Fatalf("Ausfall: %v", err)
	}
	// ... aber nur bis zum Ablauf.
	if _, err := loadRemote(ctx, c, bs, now.Add(sign.TTL+time.Hour)); err == nil {
		t.Error("abgelaufene Konfiguration muss abgelehnt werden")
	}
	up = true
	// Pfad außerhalb von roots (signiert, aber nicht erlaubt): die zwischengespeicherte bleibt in Kraft.
	serve, issued = evil, now.Add(time.Hour)
	if r, err := loadRemote(ctx, c, bs, now.Add(2*time.Hour)); err != nil || r.Sites[0].Path != "/var/www/kunde" {
		t.Errorf("außerhalb roots: %v %+v", err, r)
	}
	// Ältere Konfiguration (Rollback) wird nicht übernommen.
	serve, issued = good, now.Add(-time.Hour)
	if _, err := loadRemote(ctx, c, bs, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err) // Rückfall auf den Zwischenspeicher
	}
	// Zwischenspeicher gelöscht: eine ältere Konfiguration darf trotzdem nicht zurückkehren.
	_ = os.Remove(filepath.Join(bs.State, cacheName))
	serve, issued = good, now.Add(-time.Hour)
	if _, err := loadRemote(ctx, c, bs, now.Add(2*time.Hour)); err == nil {
		t.Error("Rollback nach Löschen des Zwischenspeichers muss abgelehnt werden")
	}
	// Falscher Schlüssel (Sitewatch-Imitation) und kein Zwischenspeicher: keine Konfiguration.
	other, _, _ := ed25519.GenerateKey(nil)
	bs2 := bs
	bs2.PublicKey, bs2.State = other, t.TempDir()
	serve, issued = good, now
	if _, err := loadRemote(ctx, c, bs2, now); err == nil {
		t.Error("falsche Signatur muss abgelehnt werden")
	}
}

func readCacheFor(bs agentcfg.Bootstrap) (any, error) {
	if _, ok := readEnvelope(filepath.Join(bs.State, cacheName)); !ok {
		return nil, context.Canceled
	}
	return nil, nil
}
