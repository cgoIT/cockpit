package main

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cockpit/internal/agentcfg"
	"cockpit/internal/agents"
	"cockpit/internal/sign"
	"cockpit/internal/status"
	"cockpit/internal/store"
	"cockpit/internal/web"
)

// TestJobEndToEnd lässt das echte Cockpit (web, SQLite) und den echten Agenten gegeneinander laufen:
// Freigabe über die Oberfläche, Abholen per Long-Poll, Ausführen, Ausgabe und Ende im Cockpit.
func TestJobEndToEnd(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "c.db"), store.DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	priv, _ := sign.LoadOrCreateKey(filepath.Join(dir, "signing.key"))
	srv := web.New(st, filepath.Join(dir, "tokens.json"), status.DefaultConfig())
	srv.SignKey, srv.Agents, srv.Jobs = priv, agents.Open(filepath.Join(dir, "agents.json")), st
	srv.Protect = func(h http.Handler) http.Handler { return h }
	srv.Who = func(*http.Request) string { return "nutzer@example.org" }
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	tok, _ := srv.Agents.Create("server1", time.Now())
	site := t.TempDir()
	_ = os.MkdirAll(filepath.Join(site, "vendor/bin"), 0o755)
	_ = os.WriteFile(filepath.Join(site, "vendor/bin/contao-console"), nil, 0o644)
	php := filepath.Join(dir, "php")
	_ = os.WriteFile(php, []byte("#!/bin/sh\necho \"Maintenance mode: $3\"\n"), 0o755)
	if err := srv.Agents.Save("server1", agentcfg.Remote{Sites: []agentcfg.Site{{ID: "cgo", Name: "cgo", Kind: "contao", Path: site, Actions: allActions("contao")}}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	bs := agentcfg.Bootstrap{Cockpit: ts.URL, Agent: "server1", Token: tok, PublicKey: priv.Public().(ed25519.PublicKey), State: t.TempDir(), Actions: []string{"maintenance-on"}, Roots: []string{os.TempDir()}}
	cur := &current{}
	cur.set(agentcfg.Config{PHP: php, Sites: []agentcfg.Site{{ID: "cgo", Kind: "contao", Path: site, Actions: allActions("contao")}}})
	kick := make(chan string, 4)
	r := newJobRunner(bs, cur, kick)

	// Der Agent wartet schon im Long-Poll, dann wird freigegeben: der Auftrag kommt ohne Verzug an.
	got := make(chan string, 1)
	go func() {
		envs, err := r.poll(context.Background())
		if err != nil || len(envs) != 1 {
			got <- "Fehler"
			return
		}
		r.handle(context.Background(), envs[0])
		got <- "fertig"
	}()
	time.Sleep(300 * time.Millisecond)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(ts.URL+"/jobs", url.Values{"site": {"cgo"}, "action": {"maintenance-on"}, "confirm": {"cgo"}})
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("Freigabe: %v %v", resp, err)
	}
	id := strings.TrimPrefix(resp.Header.Get("Location"), "/jobs/")
	select {
	case s := <-got:
		if s != "fertig" {
			t.Fatal(s)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Auftrag kam nicht beim Agenten an oder lief nicht durch")
	}
	j, _, _ := st.GetJob(id)
	chunks, _ := st.Chunks(id, 0)
	if j.Status != store.JobOK || j.ExitCode == nil || *j.ExitCode != 0 || !strings.Contains(store.Text(chunks), "Maintenance mode: enable") {
		t.Errorf("%+v\n%q", j, store.Text(chunks))
	}
	if len(kick) != 1 {
		t.Error("Site muss neu gemeldet werden")
	}
}
