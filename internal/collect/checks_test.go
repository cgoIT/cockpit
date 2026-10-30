package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTLSExpiry(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	got, verr, err := tlsExpiry(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.Before(time.Now()) {
		t.Errorf("Ablaufdatum %v liegt in der Vergangenheit", got)
	}
	if verr == nil { // das Testzertifikat ist selbstsigniert
		t.Error("Prüffehler für selbstsigniertes Zertifikat erwartet")
	}
}

func touch(t *testing.T, dir, name string, ts time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
}

func TestNewestMod(t *testing.T) {
	dir := t.TempDir()
	want := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	touch(t, dir, "alt.tar.gz", want.Add(-48*time.Hour))
	touch(t, dir, "neu.tar.gz", want)
	touch(t, dir, ".gitkeep", want.Add(24*time.Hour))                       // darf mit Maske nicht zählen
	if err := os.Mkdir(filepath.Join(dir, "x.tar.gz"), 0o700); err != nil { // Verzeichnis zählt nie
		t.Fatal(err)
	}

	got, n, err := newestMod(dir, "*.tar.gz")
	if err != nil || n != 2 || !got.Equal(want) {
		t.Errorf("Maske: got %v, %d, %v", got, n, err)
	}
	if _, n, _ := newestMod(dir, ""); n != 3 {
		t.Errorf("ohne Maske: %d Treffer, erwartet 3", n)
	}
	if _, n, err := newestMod(dir, "*.zip"); err != nil || n != 0 {
		t.Errorf("keine Treffer: %d, %v", n, err)
	}
	if _, _, err := newestMod(dir, "[x"); err == nil {
		t.Error("ungültige Maske: Fehler erwartet")
	}
}

func TestCollectChecks(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	site, backups := t.TempDir(), t.TempDir()
	touch(t, backups, "db.tar.gz", time.Now())

	// HTTPS in Großschreibung: das Schema muss trotzdem erkannt werden.
	r, err := Collect(context.Background(), Config{
		SiteID: "x", Kind: "php", Path: site, URL: "HTTPS" + strings.TrimPrefix(srv.URL, "https"),
		BackupDir: backups, BackupGlob: "*.tar.gz",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Site.TLSExpires == nil || r.Site.LastBackup == nil || r.Site.BackupCount == nil || *r.Site.BackupCount != 1 {
		t.Fatalf("Felder nicht gefüllt: %+v", r.Site)
	}
	if !strings.Contains(strings.Join(r.Site.Problems, "|"), "nicht vertrauenswürdig") {
		t.Errorf("Hinweis auf unvertrauenswürdiges Zertifikat fehlt: %v", r.Site.Problems)
	}
}
