package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sitewatch/internal/agentcfg"
	"sitewatch/internal/jobs"
	"sitewatch/internal/sign"
)

// fakeSitewatch zeichnet die Meldungen des Agenten auf. cancelOn: ab diesem Zustand/dieser Anzahl Meldungen antwortet es mit cancel.
type fakeSitewatch struct {
	mu       sync.Mutex
	events   []event
	cancelAt string // "running" = beim Start, "chunks" = bei der ersten Ausgabe-Meldung
}

func (f *fakeSitewatch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var ev event
	_ = json.NewDecoder(r.Body).Decode(&ev)
	f.mu.Lock()
	f.events = append(f.events, ev)
	cancel := (f.cancelAt == "running" && ev.State == "running") || (f.cancelAt == "chunks" && ev.State == "" && len(ev.Chunks) > 0)
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]bool{"cancel": cancel})
}

func (f *fakeSitewatch) states() (s []string, out string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.State != "" {
			s = append(s, e.State)
		}
		for _, c := range e.Chunks {
			out += c.Data
		}
	}
	return
}

func setupRunner(t *testing.T, actions []string, phpScript string, f *fakeSitewatch) (*jobRunner, ed25519.PrivateKey, chan string, string) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	site := t.TempDir()
	if err := os.MkdirAll(filepath.Join(site, "vendor/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "vendor/bin/contao-console"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	php := filepath.Join(t.TempDir(), "php")
	if err := os.WriteFile(php, []byte("#!/bin/sh\n"+phpScript), 0o755); err != nil {
		t.Fatal(err)
	}
	bs := agentcfg.Bootstrap{Sitewatch: ts.URL, Agent: "server1", Token: "tok", PublicKey: pub, State: t.TempDir(), Actions: actions, Roots: []string{os.TempDir()}}
	cur := &current{}
	cur.set(agentcfg.Config{PHP: php, Sites: []agentcfg.Site{{ID: "cgo", Kind: "contao", Path: site, Actions: allActions("contao")}, {ID: "plain", Kind: "php", Path: site, Actions: allActions("php")}}})
	kick := make(chan string, 8)
	return newJobRunner(bs, cur, kick), priv, kick, site
}

func jobFor(t *testing.T, priv ed25519.PrivateKey, id, site, action string) sign.Envelope {
	t.Helper()
	now := time.Now()
	env, err := sign.Job(priv, sign.JobPayload{ID: id, Agent: "server1", Site: site, Action: action, IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestJobRunsAndStreams(t *testing.T) {
	f := &fakeSitewatch{}
	r, priv, kick, site := setupRunner(t, []string{"maintenance-on", "maintenance-off"}, `echo "args: $@"; echo "fehler" >&2; echo "dir: $PWD"; exit 0`, f)
	r.handle(context.Background(), jobFor(t, priv, "j1", "cgo", "maintenance-on"))
	st, out := f.states()
	if strings.Join(st, ",") != "running,ok" {
		t.Fatalf("Zustände: %v", st)
	}
	for _, want := range []string{"args: vendor/bin/contao-console contao:maintenance-mode enable", "fehler", "dir: " + site} {
		if !strings.Contains(out, want) {
			t.Errorf("Ausgabe enthält %q nicht:\n%s", want, out)
		}
	}
	if len(kick) != 1 || <-kick != "cgo" {
		t.Error("die Site muss danach neu gemeldet werden")
	}
	// Dieselbe ID noch einmal (Wiederholung/Replay): nicht noch einmal ausführen.
	f.mu.Lock()
	f.events = nil
	f.mu.Unlock()
	r.handle(context.Background(), jobFor(t, priv, "j1", "cgo", "maintenance-on"))
	if st, _ := f.states(); strings.Join(st, ",") != "rejected" {
		t.Errorf("Replay: %v", st)
	}
}

func TestJobFailureReportsExit(t *testing.T) {
	f := &fakeSitewatch{}
	r, priv, _, _ := setupRunner(t, []string{"maintenance-on"}, `echo boom; exit 3`, f)
	r.handle(context.Background(), jobFor(t, priv, "j1", "cgo", "maintenance-on"))
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.events[len(f.events)-1]
	if last.State != "failed" || last.Exit == nil || *last.Exit != 3 {
		t.Errorf("%+v", last)
	}
}

func TestJobRejections(t *testing.T) {
	f := &fakeSitewatch{}
	r, priv, _, site := setupRunner(t, []string{"maintenance-on"}, `echo ran`, f) // läuft etwas, steht "ran" in der Ausgabe
	_, otherKey, _ := ed25519.GenerateKey(nil)
	foreign, _ := sign.Job(otherKey, sign.JobPayload{
		ID: "x1", Agent: "server1", Site: "cgo", Action: "maintenance-on", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)})
	expired, _ := sign.Job(priv, sign.JobPayload{ID: "x2", Agent: "server1", Site: "cgo", Action: "maintenance-on", IssuedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute)})
	for name, env := range map[string]sign.Envelope{
		"fremder Schlüssel":    foreign,
		"abgelaufen":           expired,
		"Aktion nicht erlaubt": jobFor(t, priv, "x3", "cgo", "maintenance-off"),
		"unbekannte Aktion":    jobFor(t, priv, "x4", "cgo", "rm-rf"),
		"Site nicht in Config": jobFor(t, priv, "x5", "fremd", "maintenance-on"),
		"Site ist kein Contao": jobFor(t, priv, "x6", "plain", "maintenance-on"),
		"anderer Agent": func() sign.Envelope {
			e, _ := sign.Job(priv, sign.JobPayload{ID: "x7", Agent: "zweiter", Site: "cgo", Action: "maintenance-on", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)})
			return e
		}(),
	} {
		f.mu.Lock()
		f.events = nil
		f.mu.Unlock()
		r.handle(context.Background(), env)
		st, out := f.states()
		if strings.Join(st, ",") != "rejected" || out != "" {
			t.Errorf("%s: Zustände %v, Ausgabe %q (nichts darf laufen)", name, st, out)
		}
	}
	// Contao-Konsole fehlt in der Site
	if err := os.Remove(filepath.Join(site, "vendor/bin/contao-console")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.events = nil
	f.mu.Unlock()
	r.handle(context.Background(), jobFor(t, priv, "y1", "cgo", "maintenance-on"))
	if st, _ := f.states(); strings.Join(st, ",") != "rejected" {
		t.Errorf("ohne Konsole: %v", st)
	}
}

func TestJobCanceledBeforeStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	f := &fakeSitewatch{cancelAt: "running"}
	r, priv, _, _ := setupRunner(t, []string{"maintenance-on"}, `touch `+marker, f)
	r.handle(context.Background(), jobFor(t, priv, "j1", "cgo", "maintenance-on"))
	if st, _ := f.states(); strings.Join(st, ",") != "running,canceled" {
		t.Errorf("Zustände: %v", st)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("der Befehl darf nach einem Abbruch beim Start nicht laufen")
	}
}

func TestJobCanceledWhileRunning(t *testing.T) {
	f := &fakeSitewatch{cancelAt: "chunks"}
	r, priv, _, _ := setupRunner(t, []string{"maintenance-on"}, `echo anfang; sleep 30; echo ende`, f)
	start := time.Now()
	r.handle(context.Background(), jobFor(t, priv, "j1", "cgo", "maintenance-on"))
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Abbruch kam erst nach %v", d)
	}
	st, out := f.states()
	if strings.Join(st, ",") != "running,canceled" || strings.Contains(out, "ende") {
		t.Errorf("Zustände %v, Ausgabe %q", st, out)
	}
}

func TestSeenFilePrunesOld(t *testing.T) {
	s := seenFile{filepath.Join(t.TempDir(), "seen")}
	now := time.Now()
	_ = s.add("alt", now.Add(-3*time.Hour), now)
	_ = s.add("neu", now.Add(5*time.Minute), now)
	if ok, _ := s.has("alt", now); ok {
		t.Error("alte IDs werden vergessen (sie sind ohnehin abgelaufen)")
	}
	if ok, _ := s.has("neu", now); !ok {
		t.Error("neue ID fehlt")
	}
}

func TestPollExplainsRedirect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.Write([]byte("<html>Login</html>"))
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer ts.Close()
	r, _, _, _ := setupRunner(t, []string{"maintenance-on"}, "", &fakeSitewatch{})
	r.bs.Sitewatch = ts.URL
	_, err := r.poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "leitet um") || strings.Contains(err.Error(), "invalid character") {
		t.Errorf("Fehlertext: %v", err)
	}
}

// siteTool legt <SITE_DIR>/bin/site an (das Werkzeug des Website-Stacks) und gibt SITE_DIR zurück.
func siteTool(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin/site"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSiteActionRunsToolInSiteDir(t *testing.T) {
	f := &fakeSitewatch{}
	r, priv, _, _ := setupRunner(t, []string{"site-status"}, ``, f)
	dir := siteTool(t, `printf '\033[32mgrün\033[0m %s\n' "$*"; echo "dir=$PWD color=$NO_COLOR"`)
	cfg, _ := r.cur.get()
	cfg.Sites[0].Env, cfg.Sites[0].Path = map[string]string{"SITE_DIR": dir}, filepath.Join(dir, "webroot")
	r.cur.set(cfg)
	r.handle(context.Background(), jobFor(t, priv, "j1", "cgo", "site-status"))
	st, out := f.states()
	if strings.Join(st, ",") != "running,ok" {
		t.Fatalf("Zustände: %v", st)
	}
	for _, want := range []string{"grün status", "color=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("Ausgabe enthält %q nicht: %q", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("Steuerzeichen müssen entfernt sein: %q", out)
	}
	if want := "dir=" + dir; !strings.Contains(out, want) && !strings.Contains(out, "dir=/private"+dir) {
		t.Errorf("das Werkzeug muss im Site-Verzeichnis laufen (%s): %q", dir, out)
	}
}

func TestSiteActionRejections(t *testing.T) {
	f := &fakeSitewatch{}
	r, priv, _, _ := setupRunner(t, []string{"site-status", "contao-cache-refresh", "db-list"}, `echo ran`, f)
	reset := func() { f.mu.Lock(); f.events = nil; f.mu.Unlock() }
	// ohne SITE_DIR in der Konfiguration der Site
	r.handle(context.Background(), jobFor(t, priv, "a1", "cgo", "site-status"))
	if st, out := f.states(); strings.Join(st, ",") != "rejected" || out != "" {
		t.Errorf("ohne SITE_DIR: %v %q", st, out)
	}
	// SITE_DIR ohne bin/site
	reset()
	cfg, _ := r.cur.get()
	empty := t.TempDir()
	cfg.Sites[0].Env, cfg.Sites[0].Path = map[string]string{"SITE_DIR": empty}, filepath.Join(empty, "webroot")
	r.cur.set(cfg)
	r.handle(context.Background(), jobFor(t, priv, "a2", "cgo", "site-status"))
	if st, _ := f.states(); strings.Join(st, ",") != "rejected" {
		t.Errorf("ohne bin/site: %v", st)
	}
	// SITE_DIR gehört nicht zur Site (das Verzeichnis einer anderen Site): abgelehnt
	reset()
	other := siteTool(t, `echo ran`)
	cfg.Sites[0].Env = map[string]string{"SITE_DIR": other}
	r.cur.set(cfg)
	r.handle(context.Background(), jobFor(t, priv, "a4", "cgo", "site-status"))
	if st, out := f.states(); strings.Join(st, ",") != "rejected" || strings.Contains(out, "ran") {
		t.Errorf("fremdes SITE_DIR: %v %q", st, out)
	}
	// bin/site ist eine Verknüpfung nach draußen (außerhalb von roots): abgelehnt
	reset()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "evil"), []byte("#!/bin/sh\necho ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := t.TempDir()
	_ = os.MkdirAll(filepath.Join(link, "bin"), 0o755)
	_ = os.Symlink(filepath.Join(outside, "evil"), filepath.Join(link, "bin/site"))
	cfg.Sites[0].Env, cfg.Sites[0].Path = map[string]string{"SITE_DIR": link}, filepath.Join(link, "webroot")
	r.cur.set(cfg)
	r.bs.Roots = []string{link}
	r.handle(context.Background(), jobFor(t, priv, "a5", "cgo", "site-status"))
	if st, out := f.states(); strings.Join(st, ",") != "rejected" || strings.Contains(out, "ran") {
		t.Errorf("Verknüpfung nach außerhalb von roots: %v %q", st, out)
	}
	r.bs.Roots = []string{os.TempDir()}
	// Aktion für beide Arten läuft auf einer PHP-Site durch
	reset()
	php := siteTool(t, `echo php-ran`)
	cfg.Sites[1].Env, cfg.Sites[1].Path = map[string]string{"SITE_DIR": php}, filepath.Join(php, "webroot")
	r.cur.set(cfg)
	r.handle(context.Background(), jobFor(t, priv, "a6", "plain", "site-status"))
	if st, out := f.states(); strings.Join(st, ",") != "running,ok" || !strings.Contains(out, "php-ran") {
		t.Errorf("site-status auf PHP-Site: %v %q", st, out)
	}
	// Aktion für Contao auf einer PHP-Site
	reset()
	r.handle(context.Background(), jobFor(t, priv, "a3", "plain", "contao-cache-refresh"))
	if st, out := f.states(); strings.Join(st, ",") != "rejected" || strings.Contains(out, "ran") {
		t.Errorf("falsche Art: %v %q", st, out)
	}
}

func TestOutBufStripsSplitEscapeSequences(t *testing.T) {
	o := &outBuf{}
	for _, part := range []string{"a\x1b", "[3", "1mrot\x1b[0", "m b\x1bcc"} { // Sequenzen über Schreibgrenzen verteilt, auch ESC + ein Zeichen
		if n, _ := o.Write([]byte(part)); n != len(part) {
			t.Fatalf("Write meldet %d statt %d", n, len(part))
		}
	}
	chunks := o.pending(10)
	got := ""
	for _, c := range chunks {
		got += c.Data
	}
	if got != "arot bc" {
		t.Errorf("%q", got)
	}
}

func TestOutBufFilterEdgeCases(t *testing.T) {
	for in, want := range map[string]string{
		"x\x1b[31\ny":          "x\ny",          // unvollständige Sequenz verschluckt den Zeilenumbruch nicht
		"\x1b\x1b[31mrot":      "rot",           // ESC ESC [
		"a\x1b]0;Titel\x07b":   "ab",            // OSC bis BEL
		"a\x1b]0;Titel\x1b\\b": "ab",            // OSC bis ESC \
		"a\x1b]offen\nb":       "a\nb",          // OSC ohne Ende bis Zeilenumbruch
		"zeile\r\nzwei":        "zeile\r\nzwei", // normaler Text bleibt
	} {
		o := &outBuf{}
		o.Write([]byte(in))
		got := ""
		for _, c := range o.pending(10) {
			got += c.Data
		}
		if got != want {
			t.Errorf("%q: %q, erwartet %q", in, got, want)
		}
	}
}

// allActions: alle Aktionen, die für Sites dieser Art möglich sind (Whitelist der Testsites).
func allActions(kind string) []string {
	var out []string
	for _, a := range jobs.Actions {
		if a.AllowsKind(kind) {
			out = append(out, a.Name)
		}
	}
	return out
}

func TestRejectsActionNotAllowedForSite(t *testing.T) {
	f := &fakeSitewatch{}
	r, priv, _, _ := setupRunner(t, []string{"site-status"}, `echo ran`, f)
	cfg, _ := r.cur.get()
	cfg.Sites[0].Actions = nil // lokal erlaubt, in Sitewatch für diese Site nicht freigegeben
	r.cur.set(cfg)
	r.handle(context.Background(), jobFor(t, priv, "w1", "cgo", "site-status"))
	if st, out := f.states(); strings.Join(st, ",") != "rejected" || strings.Contains(out, "ran") {
		t.Errorf("ohne Freigabe der Site: %v %q", st, out)
	}
}

func TestRefreshesConfigBeforeJudgingJob(t *testing.T) {
	f := &fakeSitewatch{}
	r, priv, _, _ := setupRunner(t, []string{"site-status"}, `echo ran`, f)
	dir := siteTool(t, `echo ran`)
	cfg, _ := r.cur.get()
	cfg.Sites[0].Env, cfg.Sites[0].Path = map[string]string{"SITE_DIR": dir}, filepath.Join(dir, "webroot")
	fresh := cfg
	fresh.Sites = append([]agentcfg.Site(nil), cfg.Sites...)
	cfg.Sites[0].Actions = nil // veralteter Stand ohne die eben in Sitewatch gespeicherte Freigabe
	r.cur.set(cfg)
	r.refresh = func(context.Context) { r.cur.set(fresh) }
	r.handle(context.Background(), jobFor(t, priv, "r1", "cgo", "site-status"))
	if st, out := f.states(); strings.Join(st, ",") != "running,ok" || !strings.Contains(out, "ran") {
		t.Errorf("mit frischer Konfiguration muss der Auftrag laufen: %v %q", st, out)
	}
}
