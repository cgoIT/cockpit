package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"cockpit/internal/agents"
	"cockpit/internal/jobs"
	"cockpit/internal/sign"
	"cockpit/internal/status"
	"cockpit/internal/store"
)

const (
	pollMax      = 20 * time.Second // kürzer als WriteTimeout des Servers (30 s)
	sseKeepAlive = 15 * time.Second
	maxChunks    = 500
	maxChunk     = 64 << 10
)

// jobRoutes: Seiten der Oberfläche. Nur mit Login (die Freigabe braucht eine Person) und mit Fernkonfiguration der Agenten.
func (s *Server) jobRoutes(ui *http.ServeMux) {
	ui.HandleFunc("GET /jobs", s.jobList)
	ui.HandleFunc("GET /jobs/new", s.jobNew)
	ui.HandleFunc("POST /jobs", s.jobCreate)
	ui.HandleFunc("GET /jobs/{id}", s.jobShow)
	ui.HandleFunc("POST /jobs/{id}/cancel", s.jobCancel)
	ui.HandleFunc("GET /jobs/{id}/stream", s.jobStream)
}

// announce: was ein Agent beim Abholen als lokal erlaubt gemeldet hat. Nur im Speicher; ein Agent meldet es alle ~20 s neu.
type announce struct {
	Actions []string
	At      time.Time
}

const announceFresh = 2 * time.Minute // so lange gilt eine Meldung des Agenten als aktuell

func (s *Server) setAnnounced(agent string, actions []string) {
	s.annMu.Lock()
	defer s.annMu.Unlock()
	if s.ann == nil {
		s.ann = map[string]announce{}
	}
	s.ann[agent] = announce{actions, s.Now()}
}

func (s *Server) announced(agent string) (announce, bool) {
	s.annMu.Lock()
	defer s.annMu.Unlock()
	a, ok := s.ann[agent]
	return a, ok && s.Now().Sub(a.At) < announceFresh
}

// actionOffered: der Agent ist erreichbar und hat die Aktion lokal erlaubt. Das Cockpit bietet nur Aufträge an, die der Agent annimmt.
func (s *Server) actionOffered(agent, action string) bool {
	a, ok := s.announced(agent)
	if ok {
		for _, x := range a.Actions {
			if x == action {
				return true
			}
		}
	}
	return false
}

// channelText beschreibt den Auftragskanal eines Agenten für die Agentenseite.
func (s *Server) channelText(agent string) string {
	if a, ok := s.announced(agent); ok {
		return "aktiv, lokal erlaubt: " + strings.Join(a.Actions, ", ") + " (zuletzt gesehen vor " + status.Ago(s.Now().Sub(a.At)) + ")"
	}
	return "nicht gemeldet (Kanal aus, weil in der bootstrap.json kein \"actions\" steht, oder der Agent ist nicht erreichbar)"
}

func (s *Server) jobsUI() bool {
	return s.Jobs != nil && s.Agents != nil && s.Protect != nil && s.Who != nil
}

func newJobID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validJobID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// --- Agent-API -----------------------------------------------------------------------------------------------

func (s *Server) bearerAgent(w http.ResponseWriter, r *http.Request) (agents.Agent, bool) {
	a, ok, err := s.Agents.ByToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil {
		log.Printf("agents: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return a, false
	}
	if !ok {
		http.Error(w, "nicht autorisiert", http.StatusUnauthorized)
	}
	return a, ok
}

// jobPoll liefert dem Agenten den ältesten wartenden Auftrag. Gibt es keinen, hält das Cockpit die Anfrage bis zu ?wait Sekunden offen
// und antwortet sofort, sobald ein Auftrag freigegeben wird (Long-Polling: der Agent baut die Verbindung aus, auf dem Server ist kein Port offen).
func (s *Server) jobPoll(w http.ResponseWriter, r *http.Request) {
	a, ok := s.bearerAgent(w, r)
	if !ok {
		return
	}
	wait := pollMax
	if n, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && n >= 0 && time.Duration(n)*time.Second < wait {
		wait = time.Duration(n) * time.Second
	}
	var acts []string // der Agent nennt, was er lokal erlaubt; das Cockpit bietet nur das an
	for _, x := range strings.Split(r.URL.Query().Get("actions"), ",") {
		if _, ok := jobs.Find(x); ok && !slices.Contains(acts, x) {
			acts = append(acts, x)
		}
	}
	s.setAnnounced(a.ID, acts)
	ch, stop := s.hub.subscribe("agent:" + a.ID)
	defer stop()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	for {
		j, found, err := s.Jobs.NextJob(a.ID, s.Now())
		if err != nil {
			log.Printf("jobs: %v", err)
			http.Error(w, "interner Fehler", http.StatusInternalServerError)
			return
		}
		if found {
			_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []json.RawMessage{j.Envelope}})
			return
		}
		select {
		case <-ch:
		case <-timer.C:
			_, _ = w.Write([]byte(`{"jobs":[]}` + "\n"))
			return
		case <-r.Context().Done():
			return
		}
	}
}

type jobEvent struct {
	State   string        `json:"state"` // "" = nur Ausgabe; running | ok | failed | canceled | rejected
	Exit    *int          `json:"exit"`
	Message string        `json:"message"`
	Chunks  []store.Chunk `json:"chunks"`
}

var finalStates = map[string]string{"ok": store.JobOK, "failed": store.JobFailed, "canceled": store.JobCanceled, "rejected": store.JobRejected}

// jobEvents nimmt Zustand und Ausgabe des Agenten entgegen. "running" ist zugleich die Rückfrage vor dem Start: Antwortet das Cockpit
// mit cancel, führt der Agent nichts aus (so läuft ein zwischenzeitlich abgebrochener Auftrag nicht mehr an).
func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	a, ok := s.bearerAgent(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	j, found, err := s.Jobs.GetJob(id)
	if err == nil && (!found || j.Agent != a.ID || !validJobID(id)) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("jobs: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var ev jobEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil || len(ev.Chunks) > maxChunks {
		http.Error(w, "ungültiges JSON", http.StatusBadRequest)
		return
	}
	if _, fin := finalStates[ev.State]; ev.State != "" && ev.State != "running" && !fin {
		http.Error(w, "unbekannter Zustand", http.StatusBadRequest)
		return
	}
	for i, c := range ev.Chunks {
		if len(c.Data) > maxChunk {
			http.Error(w, "Block zu groß", http.StatusRequestEntityTooLarge)
			return
		}
		ev.Chunks[i].Data = strings.ToValidUTF8(c.Data, "�")
	}
	now := s.Now()
	cancel := false
	fail := func(err error) {
		log.Printf("jobs: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
	}
	if ev.State == "running" {
		ok, err := s.Jobs.StartJob(id, now)
		if err != nil {
			fail(err)
			return
		}
		cancel = !ok
	}
	if len(ev.Chunks) > 0 || ev.State == "" || ev.State == "running" {
		c, err := s.Jobs.AppendChunks(id, ev.Chunks)
		if err != nil {
			fail(err)
			return
		}
		cancel = cancel || c
	}
	if st, fin := finalStates[ev.State]; fin {
		msg := strings.ToValidUTF8(ev.Message, "�")
		if len(msg) > 500 {
			msg = strings.ToValidUTF8(msg[:500], "")
		}
		if err := s.Jobs.FinishJob(id, st, msg, ev.Exit, now); err != nil {
			fail(err)
			return
		}
		log.Printf("jobs: %s %s auf %q beendet: %s", id, j.Action, j.Site, st)
	}
	s.hub.notify("job:" + id)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]bool{"cancel": cancel})
}

// --- Oberfläche ----------------------------------------------------------------------------------------------

type jobRow struct {
	ID, Site, Action, Label, By, Created string
	Status, Class                        string
}

// jobStatus: Anzeige-Text und CSS-Klasse. "abgelaufen" ergibt sich aus Zeit und Zustand "wartend".
func jobStatus(j store.Job, now time.Time) (label, class string) {
	switch {
	case j.Status == store.JobQueued && !now.Before(j.ExpiresAt):
		return "abgelaufen (nicht abgeholt)", "off"
	case j.Status == store.JobQueued:
		return "wartet auf den Agenten", "info"
	case j.Hung(now):
		return "Agent meldet sich nicht mehr", "crit"
	case j.Status == store.JobRunning && j.CancelRequested:
		return "Abbruch angefordert", "warn"
	case j.Status == store.JobRunning:
		return "läuft", "info"
	case j.Status == store.JobOK:
		return "erfolgreich", "ok"
	case j.Status == store.JobFailed:
		return "fehlgeschlagen", "crit"
	case j.Status == store.JobCanceled:
		return "abgebrochen", "off"
	case j.Status == store.JobRejected:
		return "vom Agenten abgelehnt", "crit"
	}
	return j.Status, "off"
}

func jobEnded(j store.Job, now time.Time) bool { return !j.Active(now) }

func actionLabel(name string) string {
	if a, ok := jobs.Find(name); ok {
		return a.Label
	}
	return name
}

type siteChoice struct {
	ID, Label, Agent, Kind string
	// Die Aktionen, die der Agent gerade angemeldet hat und die zur Art der Site passen, nach Stufe getrennt.
	Read, Change, Critical []jobs.Action
}

func (c siteChoice) Has(action string) bool {
	for _, l := range [][]jobs.Action{c.Read, c.Change, c.Critical} {
		if slices.ContainsFunc(l, func(a jobs.Action) bool { return a.Name == action }) {
			return true
		}
	}
	return false
}

func (c siteChoice) Any() bool { return len(c.Read)+len(c.Change)+len(c.Critical) > 0 }

// contaoSites: die Sites, für die Aufträge möglich sind (Contao oder PHP, einem Agenten zugewiesen).
func (s *Server) contaoSites() ([]siteChoice, error) {
	as, err := s.Agents.List()
	if err != nil {
		return nil, err
	}
	var out []siteChoice
	for _, a := range as {
		for _, st := range a.Config.Sites {
			if st.Kind == "host" {
				continue
			}
			c := siteChoice{ID: st.ID, Label: st.ID + " (" + a.ID + ")", Agent: a.ID, Kind: st.Kind}
			for _, act := range jobs.Actions {
				if !act.AllowsKind(st.Kind) || !slices.Contains(st.Actions, act.Name) || !s.actionOffered(a.ID, act.Name) ||
					(strings.HasPrefix(act.Name, "db-") && st.MariaDB != nil && !*st.MariaDB) { // ohne Datenbank keine DB-Aktionen
					continue
				}
				switch act.Level {
				case jobs.Direct:
					c.Read = append(c.Read, act)
				case jobs.Confirm:
					c.Change = append(c.Change, act)
				default:
					c.Critical = append(c.Critical, act)
				}
			}
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Server) siteChoice(site string) (siteChoice, bool, error) {
	cs, err := s.contaoSites()
	for _, c := range cs {
		if c.ID == site {
			return c, true, err
		}
	}
	return siteChoice{}, false, err
}

func (s *Server) jobList(w http.ResponseWriter, r *http.Request) {
	list, err := s.Jobs.ListJobs(50)
	sites, err2 := s.contaoSites()
	if err != nil || err2 != nil {
		log.Printf("jobs: %v %v", err, err2)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	now := s.Now()
	var rows []jobRow
	for _, j := range list {
		lb, cl := jobStatus(j, now)
		rows = append(rows, jobRow{j.ID, j.Site, j.Action, actionLabel(j.Action), j.ApprovedBy, fmtStamp(j.CreatedAt), lb, cl})
	}
	s.render(w, "jobs.html", map[string]any{"Rows": rows, "Sites": sites, "Top": "jobs", "Error": r.URL.Query().Get("error")})
}

const errNotAllowedForSite = "Die Aktion ist für diese Site nicht freigegeben (Agentenseite, Abschnitt der Site)."

func errNotOffered(agent, action string) string {
	return fmt.Sprintf("Agent %q hat die Aktion %q nicht angemeldet: Auftragskanal aus, Aktion nicht in actions der bootstrap.json oder Agent nicht erreichbar.", agent, action)
}

func fmtStamp(t time.Time) string { return t.Local().Format("02.01.2006 15:04:05") }

func (s *Server) jobNew(w http.ResponseWriter, r *http.Request) {
	s.jobConfirmPage(w, r.URL.Query().Get("site"), r.URL.Query().Get("action"), "", http.StatusOK)
}

func (s *Server) jobConfirmPage(w http.ResponseWriter, site, action, msg string, code int) {
	a, okA := jobs.Find(action)
	c, okS, err := s.siteChoice(site)
	if err != nil {
		log.Printf("jobs: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	if !okA || !okS || !a.AllowsKind(c.Kind) {
		http.Error(w, "unbekannte Site oder Aktion (oder die Aktion gilt nicht für diese Art von Site)", http.StatusBadRequest)
		return
	}
	if !s.actionOffered(c.Agent, action) {
		http.Error(w, errNotOffered(c.Agent, action), http.StatusConflict)
		return
	}
	if !c.Has(action) {
		http.Error(w, errNotAllowedForSite, http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8") // vor WriteHeader, sonst wirkt es nicht mehr
	w.WriteHeader(code)
	s.render(w, "job_new.html", map[string]any{"Site": site, "Agent": c.Agent, "Action": a, "Minutes": int(jobs.TTL.Minutes()), "Error": msg, "Top": "jobs"})
}

func (s *Server) jobCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungültige Eingabe", http.StatusBadRequest)
		return
	}
	site, action := r.PostForm.Get("site"), r.PostForm.Get("action")
	a, okA := jobs.Find(action)
	c, okS, err := s.siteChoice(site)
	if err != nil {
		log.Printf("jobs: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	who := s.Who(r)
	switch {
	case !okA || !okS || !a.AllowsKind(c.Kind):
		http.Error(w, "unbekannte Site oder Aktion (oder die Aktion gilt nicht für diese Art von Site)", http.StatusBadRequest)
		return
	case who == "":
		http.Error(w, "nicht angemeldet", http.StatusForbidden)
		return
	case !s.actionOffered(c.Agent, action):
		http.Error(w, errNotOffered(c.Agent, action), http.StatusConflict)
		return
	case !c.Has(action):
		http.Error(w, errNotAllowedForSite, http.StatusConflict)
		return
	case a.NeedsID() && strings.TrimSpace(r.PostForm.Get("confirm")) != site:
		s.jobConfirmPage(w, site, action, "Die Site-ID stimmt nicht. Zur Bestätigung muss sie genau eingetragen werden.", http.StatusUnprocessableEntity)
		return
	}
	now := s.Now()
	id := newJobID()
	env, err := sign.Job(s.SignKey, sign.JobPayload{ID: id, Agent: c.Agent, Site: site, Action: action, IssuedAt: now, ExpiresAt: now.Add(jobs.TTL)})
	if err != nil {
		log.Printf("jobs: signieren: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	raw, _ := json.Marshal(env)
	err = s.Jobs.CreateJob(store.Job{ID: id, Agent: c.Agent, Site: site, Action: action, ApprovedBy: who, CreatedAt: now, ExpiresAt: now.Add(jobs.TTL), Envelope: raw})
	if errors.Is(err, store.ErrJobBusy) {
		s.jobConfirmPage(w, site, action, err.Error()+". Erst abwarten oder abbrechen.", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("jobs: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	log.Printf("jobs: %s %s auf %q (Agent %q) freigegeben von %q", id, action, site, c.Agent, who)
	s.hub.notify("agent:" + c.Agent)
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

func (s *Server) loadJob(w http.ResponseWriter, r *http.Request) (store.Job, bool) {
	id := r.PathValue("id")
	if !validJobID(id) {
		http.NotFound(w, r)
		return store.Job{}, false
	}
	j, ok, err := s.Jobs.GetJob(id)
	if err != nil {
		log.Printf("jobs: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return j, false
	}
	if !ok {
		http.NotFound(w, r)
	}
	return j, ok
}

func (s *Server) jobShow(w http.ResponseWriter, r *http.Request) {
	j, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	chunks, err := s.Jobs.Chunks(j.ID, 0)
	if err != nil {
		log.Printf("jobs: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	last := 0
	if len(chunks) > 0 {
		last = chunks[len(chunks)-1].Seq
	}
	now := s.Now()
	lb, cl := jobStatus(j, now)
	nonce := newJobID()
	// Nur diese Seite darf ein Skript ausführen (Nonce), und nur zur eigenen Adresse (EventSource). Alle anderen Seiten bleiben ohne JavaScript.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; form-action 'self'")
	s.render(w, "job.html", map[string]any{
		"Job": j, "Label": actionLabel(j.Action), "Status": lb, "Class": cl, "Output": store.Text(chunks), "Last": last,
		"Live": !jobEnded(j, now), "Nonce": nonce, "Created": fmtStamp(j.CreatedAt), "Top": "jobs",
		"Started": stamp(j.StartedAt), "Finished": stamp(j.FinishedAt), "Expires": fmtStamp(j.ExpiresAt),
	})
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	return fmtStamp(t)
}

func (s *Server) jobCancel(w http.ResponseWriter, r *http.Request) {
	j, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	if err := s.Jobs.CancelJob(j.ID, s.Now()); err != nil {
		log.Printf("jobs: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	log.Printf("jobs: %s abgebrochen von %q", j.ID, s.Who(r))
	s.hub.notify("job:" + j.ID)
	http.Redirect(w, r, "/jobs/"+j.ID, http.StatusSeeOther)
}

// jobStream überträgt die Ausgabe live (Server-Sent Events). Die Ereignis-ID ist die Seq des Blocks: Nach einem Verbindungsabbruch
// meldet der Browser selbst die letzte erhaltene ID, und es geht ohne Lücke und ohne Doppelte weiter.
func (s *Server) jobStream(w http.ResponseWriter, r *http.Request) {
	j, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		if n, err := strconv.Atoi(h); err == nil {
			after = n
		}
	}
	rc := http.NewResponseController(w)
	// Die Zeitlimits des Servers (30 s) gelten für kurze Anfragen, nicht für diesen Strom.
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.SetReadDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx soll nicht puffern
	ch, stop := s.hub.subscribe("job:" + j.ID)
	defer stop()
	lastLabel := ""
	for {
		cur, found, err := s.Jobs.GetJob(j.ID)
		if err != nil || !found {
			return
		}
		chunks, err := s.Jobs.Chunks(j.ID, after)
		if err != nil {
			return
		}
		for _, c := range chunks {
			data, _ := json.Marshal(c.Data)
			fmt.Fprintf(w, "id: %d\nevent: out\ndata: %s\n\n", c.Seq, data)
			after = c.Seq
		}
		now := s.Now()
		if lb, cl := jobStatus(cur, now); lb != lastLabel {
			lastLabel = lb
			data, _ := json.Marshal(map[string]string{"label": lb, "class": cl})
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", data)
		}
		if jobEnded(cur, now) {
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
			_ = rc.Flush()
			return
		}
		if rc.Flush() != nil {
			return
		}
		select {
		case <-ch:
		case <-time.After(sseKeepAlive): // Lebenszeichen, auch damit ein auslaufender Auftrag ("abgelaufen") bemerkt wird
			fmt.Fprint(w, ": ka\n\n")
		case <-r.Context().Done():
			return
		}
	}
}
