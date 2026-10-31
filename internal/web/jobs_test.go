package web

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"sitewatch/internal/agentcfg"
	"sitewatch/internal/jobs"
	"sitewatch/internal/sign"
	"sitewatch/internal/store"
)

func setupJobs(t *testing.T) (srv *Server, tok string) {
	t.Helper()
	srv, _ = setupAgents(t)
	srv.Protect = func(h http.Handler) http.Handler { return h }
	srv.Who = func(*http.Request) string { return "nutzer@example.org" }
	srv.Jobs = srv.Store.(store.Jobs)
	var err error
	if tok, err = srv.Agents.Create("server1", srv.Now()); err != nil {
		t.Fatal(err)
	}
	cfg := agentcfg.Remote{Sites: []agentcfg.Site{{ID: "cgo", Name: "cgo", Kind: "contao", Path: "/srv/cgo", Actions: allActions("contao")}, {ID: "plain", Name: "plain", Kind: "php", Path: "/srv/plain", Actions: allActions("php")}}}
	if err := srv.Agents.Save("server1", cfg, srv.Now()); err != nil {
		t.Fatal(err)
	}
	agentAnnounces(srv, tok, "maintenance-on,maintenance-off")
	return srv, tok
}

// agentAnnounces lässt den Agenten beim Abholen melden, welche Aktionen er lokal erlaubt.
func agentAnnounces(srv *Server, tok, actions string) {
	do(srv, "GET", "/api/v1/agent/jobs?wait=0&actions="+url.QueryEscape(actions), nil, bearer(tok))
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func createJob(t *testing.T, srv *Server, action string) string {
	t.Helper()
	w := do(srv, "POST", "/jobs", url.Values{"site": {"cgo"}, "action": {action}, "confirm": {"cgo"}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("Freigabe: %d %s", w.Code, w.Body.String())
	}
	return strings.TrimPrefix(w.Header().Get("Location"), "/jobs/")
}

func event(t *testing.T, srv *Server, tok, id string, ev jobEvent) map[string]bool {
	t.Helper()
	b, _ := json.Marshal(ev)
	req := httptest.NewRequest("POST", "/api/v1/agent/jobs/"+id+"/events", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("events: %d %s", w.Code, w.Body.String())
	}
	var out map[string]bool
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func TestJobFlow(t *testing.T) {
	srv, tok := setupJobs(t)
	if w := do(srv, "GET", "/jobs/new?site=cgo&action=maintenance-on", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Site-ID") {
		t.Fatalf("Bestätigungsseite: %d", w.Code)
	}
	if w := do(srv, "GET", "/jobs/new?site=plain&action=maintenance-on", nil, nil); w.Code != 400 {
		t.Errorf("nur Contao-Sites: %d", w.Code)
	}
	if w := do(srv, "POST", "/jobs", url.Values{"site": {"cgo"}, "action": {"maintenance-on"}, "confirm": {"falsch"}}, nil); w.Code != 422 {
		t.Errorf("falsche Bestätigung: %d", w.Code)
	}
	if w := do(srv, "POST", "/jobs", url.Values{"site": {"cgo"}, "action": {"rm-rf"}, "confirm": {"cgo"}}, nil); w.Code != 400 {
		t.Errorf("unbekannte Aktion: %d", w.Code)
	}
	id := createJob(t, srv, "maintenance-on")
	if w := do(srv, "POST", "/jobs", url.Values{"site": {"cgo"}, "action": {"maintenance-off"}}, nil); w.Code != 409 {
		t.Errorf("zweiter Auftrag für dieselbe Site: %d", w.Code)
	}

	// Agent holt den Auftrag ab: fremdes Token bekommt nichts, die Signatur stimmt.
	if w := do(srv, "GET", "/api/v1/agent/jobs?wait=0", nil, bearer("falsch")); w.Code != 401 {
		t.Errorf("ohne gültiges Token: %d", w.Code)
	}
	w := do(srv, "GET", "/api/v1/agent/jobs?wait=0", nil, bearer(tok))
	var got struct{ Jobs []sign.Envelope }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Jobs) != 1 {
		t.Fatalf("Abholen: %d %s", w.Code, w.Body.String())
	}
	pub := srv.SignKey.Public().(ed25519.PublicKey)
	p, err := sign.VerifyJob(pub, got.Jobs[0], "server1", srv.Now())
	if err != nil || p.ID != id || p.Site != "cgo" || p.Action != "maintenance-on" {
		t.Fatalf("Auftrag: %+v %v", p, err)
	}
	if _, err := sign.VerifyJob(pub, got.Jobs[0], "anderer", srv.Now()); err == nil {
		t.Error("anderer Empfänger muss scheitern")
	}

	// Start, Ausgabe, Ende.
	if ev := event(t, srv, tok, id, jobEvent{State: "running"}); ev["cancel"] {
		t.Fatal("Start verweigert")
	}
	event(t, srv, tok, id, jobEvent{Chunks: []store.Chunk{{Seq: 1, Data: "Maintenance <b>on</b>\n"}}})
	page := do(srv, "GET", "/jobs/"+id, nil, nil)
	csp := page.Header().Get("Content-Security-Policy")
	_, rest, _ := strings.Cut(csp, "script-src 'nonce-")
	nonce, _, _ := strings.Cut(rest, "'")
	if !strings.Contains(page.Body.String(), "Maintenance &lt;b&gt;on&lt;/b&gt;") || !strings.Contains(page.Body.String(), `nonce="`+nonce+`"`) || !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("Auftragsseite: %s\n%s", csp, page.Body.String())
	}
	exit := 0
	event(t, srv, tok, id, jobEvent{State: "ok", Exit: &exit, Message: "fertig"})
	j, _, _ := srv.Jobs.GetJob(id)
	if j.Status != store.JobOK || j.ExitCode == nil || *j.ExitCode != 0 || j.ApprovedBy != "nutzer@example.org" {
		t.Errorf("%+v", j)
	}
	if w := do(srv, "GET", "/jobs", nil, nil); !strings.Contains(w.Body.String(), "erfolgreich") {
		t.Error("Liste zeigt den Status nicht")
	}
	// Fremder Agent sieht den Auftrag nicht.
	other, _ := srv.Agents.Create("zweiter", srv.Now())
	b, _ := json.Marshal(jobEvent{State: "running"})
	req := httptest.NewRequest("POST", "/api/v1/agent/jobs/"+id+"/events", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+other)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("fremder Agent: %d", rec.Code)
	}
}

func TestJobEventsRejectsBadInput(t *testing.T) {
	srv, tok := setupJobs(t)
	id := createJob(t, srv, "maintenance-on")
	post := func(body string) int {
		req := httptest.NewRequest("POST", "/api/v1/agent/jobs/"+id+"/events", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w.Code
	}
	if c := post(`{"state":"exploded"}`); c != 400 {
		t.Errorf("unbekannter Zustand: %d", c)
	}
	if c := post(`kein json`); c != 400 {
		t.Errorf("kaputtes JSON: %d", c)
	}
	big, _ := json.Marshal(jobEvent{Chunks: []store.Chunk{{Seq: 1, Data: strings.Repeat("x", maxChunk+1)}}})
	if c := post(string(big)); c != 413 {
		t.Errorf("zu großer Block: %d", c)
	}
	if j, _, _ := srv.Jobs.GetJob(id); j.Status != store.JobQueued {
		t.Errorf("abgelehnte Meldungen dürfen nichts ändern: %s", j.Status)
	}
}

func TestJobCancelBeforeStart(t *testing.T) {
	srv, tok := setupJobs(t)
	id := createJob(t, srv, "maintenance-on")
	if w := do(srv, "POST", "/jobs/"+id+"/cancel", url.Values{}, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("Abbrechen: %d", w.Code)
	}
	if ev := event(t, srv, tok, id, jobEvent{State: "running"}); !ev["cancel"] {
		t.Error("abgebrochener Auftrag darf nicht starten")
	}
	if w := do(srv, "GET", "/api/v1/agent/jobs?wait=0", nil, bearer(tok)); strings.Contains(w.Body.String(), "payload") {
		t.Error("abgebrochener Auftrag wird nicht mehr ausgeliefert")
	}
}

func TestJobCancelWhileRunning(t *testing.T) {
	srv, tok := setupJobs(t)
	id := createJob(t, srv, "maintenance-on")
	event(t, srv, tok, id, jobEvent{State: "running"})
	do(srv, "POST", "/jobs/"+id+"/cancel", url.Values{}, nil)
	if ev := event(t, srv, tok, id, jobEvent{}); !ev["cancel"] {
		t.Error("laufender Auftrag muss den Abbruch erfahren")
	}
}

func TestJobsDisabledWithoutLogin(t *testing.T) {
	srv, _ := setupJobs(t)
	srv.Protect = nil
	if w := do(srv, "GET", "/jobs", nil, nil); w.Code != 404 {
		t.Errorf("ohne Login darf es keine Seiten für Aufträge geben: %d", w.Code)
	}
}

func TestJobLongPollWakesUp(t *testing.T) {
	srv, tok := setupJobs(t)
	done := make(chan string, 1)
	go func() {
		done <- do(srv, "GET", "/api/v1/agent/jobs?wait=15&actions=maintenance-on", nil, bearer(tok)).Body.String()
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	createJob(t, srv, "maintenance-on")
	select {
	case body := <-done:
		if !strings.Contains(body, "payload") || time.Since(start) > 2*time.Second {
			t.Errorf("Long-Poll: %s nach %v", body, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Long-Poll wurde nicht geweckt")
	}
}

func TestJobStream(t *testing.T) {
	srv, tok := setupJobs(t)
	id := createJob(t, srv, "maintenance-on")
	event(t, srv, tok, id, jobEvent{State: "running", Chunks: []store.Chunk{{Seq: 1, Data: "eins\n"}}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/jobs/"+id+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type %q", resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	wait := func(want string) {
		t.Helper()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("Strom zu Ende, erwartet %q", want)
				}
				if strings.Contains(l, want) {
					return
				}
			case <-ctx.Done():
				t.Fatalf("Zeit abgelaufen, erwartet %q", want)
			}
		}
	}
	wait(`"eins\n"`) // der schon vorhandene Block kommt sofort
	event(t, srv, tok, id, jobEvent{Chunks: []store.Chunk{{Seq: 2, Data: "zwei\n"}}})
	wait(`"zwei\n"`) // ein neuer Block kommt ohne Neuladen
	exit := 0
	event(t, srv, tok, id, jobEvent{State: "ok", Exit: &exit})
	wait("event: end")
}

func TestOnlyAnnouncedActionsAreOffered(t *testing.T) {
	srv, tok := setupJobs(t)
	if w := do(srv, "GET", "/jobs", nil, nil); !strings.Contains(w.Body.String(), "action=maintenance-on") || !strings.Contains(w.Body.String(), "action=maintenance-off") {
		t.Errorf("beide Aktionen sollten angeboten werden: %s", w.Body.String())
	}
	if w := do(srv, "GET", "/agents/server1", nil, nil); !strings.Contains(w.Body.String(), "lokal erlaubt: maintenance-on, maintenance-off") {
		t.Error("Agentenseite zeigt die gemeldete Erlaubnis nicht")
	}
	// Der Agent erlaubt nur noch das Einschalten (und nennt eine unbekannte Aktion, die ignoriert wird).
	agentAnnounces(srv, tok, "maintenance-on,rm-rf")
	if w := do(srv, "GET", "/jobs", nil, nil); strings.Contains(w.Body.String(), "action=maintenance-off") || strings.Contains(w.Body.String(), "rm-rf") {
		t.Error("nicht erlaubte Aktion wird angeboten")
	}
	if w := do(srv, "GET", "/jobs/new?site=cgo&action=maintenance-off", nil, nil); w.Code != 409 {
		t.Errorf("Bestätigungsseite für nicht angemeldete Aktion: %d", w.Code)
	}
	if w := do(srv, "POST", "/jobs", url.Values{"site": {"cgo"}, "action": {"maintenance-off"}}, nil); w.Code != 409 {
		t.Errorf("Freigabe einer nicht angemeldeten Aktion: %d", w.Code)
	}
	// Meldet sich der Agent länger nicht, ist er nicht erreichbar: nichts wird mehr angeboten.
	late := srv.Now().Add(announceFresh + time.Second)
	srv.Now = func() time.Time { return late }
	if w := do(srv, "GET", "/jobs", nil, nil); strings.Contains(w.Body.String(), "action=maintenance-on") {
		t.Error("nach Ablauf der Meldung darf nichts angeboten werden")
	}
	if w := do(srv, "GET", "/agents/server1", nil, nil); !strings.Contains(w.Body.String(), "nicht gemeldet") {
		t.Error("Agentenseite muss den fehlenden Kanal zeigen")
	}
}

func TestTokenBlockMentionsActions(t *testing.T) {
	srv, _ := setupJobs(t)
	w := do(srv, "POST", "/agents", url.Values{"id": {"neu"}}, nil)
	if !strings.Contains(w.Body.String(), `"actions": []`) {
		t.Errorf("Block für die bootstrap.json ohne actions: %s", w.Body.String())
	}
}

func TestActionsByLevelAndKind(t *testing.T) {
	srv, tok := setupJobs(t)
	agentAnnounces(srv, tok, "site-status,contao-cache-refresh,contao-update,db-list,maintenance-on")
	body := do(srv, "GET", "/jobs", nil, nil).Body.String()
	// Lesende Aktionen sind ein Knopf in einem Formular (POST), nie ein Link: Ein GET darf nichts auslösen.
	if !strings.Contains(body, `name="action" value="site-status"`) || strings.Contains(body, "action=site-status") {
		t.Error("lesende Aktion muss ein POST-Knopf sein")
	}
	if !strings.Contains(body, "action=contao-cache-refresh") || !strings.Contains(body, "action=contao-update") {
		t.Error("ändernde Aktionen führen über die Bestätigungsseite")
	}
	// Die PHP-Site bekommt nur Aktionen, die für PHP gelten.
	if w := do(srv, "POST", "/jobs", url.Values{"site": {"plain"}, "action": {"contao-cache-refresh"}}, nil); w.Code != 400 {
		t.Errorf("Contao-Aktion auf PHP-Site: %d", w.Code)
	}
	if w := do(srv, "POST", "/jobs", url.Values{"site": {"plain"}, "action": {"db-list"}}, nil); w.Code != http.StatusSeeOther {
		t.Errorf("db-list auf PHP-Site: %d", w.Code)
	}
	// Direkt ausgelöst, ohne Bestätigung; contao-update dagegen verlangt die Site-ID.
	if w := do(srv, "POST", "/jobs", url.Values{"site": {"cgo"}, "action": {"site-status"}}, nil); w.Code != http.StatusSeeOther {
		t.Errorf("site-status: %d", w.Code)
	}
	if w := do(srv, "POST", "/jobs", url.Values{"site": {"cgo"}, "action": {"contao-update"}}, nil); w.Code != 422 {
		t.Errorf("contao-update ohne Site-ID: %d", w.Code)
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

func TestSiteWhitelistLimitsOffers(t *testing.T) {
	srv, tok := setupJobs(t)
	agentAnnounces(srv, tok, "site-status,site-restart,db-snapshot")
	no := false
	cfg := agentcfg.Remote{Sites: []agentcfg.Site{
		{ID: "cgo", Kind: "contao", Path: "/srv/cgo", Actions: []string{"site-status", "site-restart", "db-snapshot"}, MariaDB: &no},
		{ID: "plain", Kind: "php", Path: "/srv/plain", Actions: []string{"site-status"}},
	}}
	if err := srv.Agents.Save("server1", cfg, srv.Now()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		site, action string
		want         int
	}{
		{"cgo", "site-restart", 200},
		{"cgo", "db-snapshot", 409},    // erlaubt, aber die Site hat keine Datenbank
		{"plain", "site-restart", 409}, // vom Agenten erlaubt, für die Site nicht freigegeben
		{"plain", "db-snapshot", 409},
	} {
		if w := do(srv, "GET", "/jobs/new?site="+c.site+"&action="+c.action, nil, nil); w.Code != c.want {
			t.Errorf("%s/%s: %d, erwartet %d", c.site, c.action, w.Code, c.want)
		}
		if c.want == 409 {
			if w := do(srv, "POST", "/jobs", url.Values{"site": {c.site}, "action": {c.action}}, nil); w.Code != 409 {
				t.Errorf("POST %s/%s: %d", c.site, c.action, w.Code)
			}
		}
	}
}

func TestAgentPageSavesSiteActions(t *testing.T) {
	srv, tok := setupJobs(t)
	agentAnnounces(srv, tok, "site-status")
	form := url.Values{"count": {"1"}, "s0_id": {"cgo"}, "s0_kind": {"contao"}, "s0_path": {"/srv/cgo"}, "s0_act": {"site-status", "erfunden"}}
	if w := do(srv, "POST", "/agents/server1", form, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("speichern: %d %s", w.Code, w.Body.String())
	}
	a, _ := srv.Agents.Get("server1")
	if got := strings.Join(a.Config.Sites[0].Actions, ","); got != "site-status" {
		t.Errorf("gespeichert: %q (Unbekanntes muss wegfallen)", got)
	}
	w := do(srv, "GET", "/agents/server1", nil, nil)
	if !strings.Contains(w.Body.String(), `name="s0_act" value="site-status" data-rep checked>`) || strings.Contains(w.Body.String(), `value="site-doctor" checked`) {
		t.Errorf("Formular zeigt die Auswahl falsch: %s", w.Body.String())
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'nonce-") || !strings.Contains(w.Body.String(), `<script nonce="`) {
		t.Errorf("Agentenseite braucht Skript mit Nonce für „Alle gemeldeten wählen“: %q", csp)
	}
	// Ohne Aufträge (-no-login) zeigt das Formular keine Auswahl; Speichern darf die Freigaben nicht löschen.
	srv.Who = nil
	form.Del("s0_act")
	if w := do(srv, "POST", "/agents/server1", form, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("speichern ohne Aufträge: %d", w.Code)
	}
	if a, _ = srv.Agents.Get("server1"); len(a.Config.Sites[0].Actions) != 1 {
		t.Errorf("Freigaben gingen verloren: %v", a.Config.Sites[0].Actions)
	}
}
