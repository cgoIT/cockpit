package web

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"sitewatch/internal/agentcfg"
	"sitewatch/internal/agents"
	"sitewatch/internal/auth"
	"sitewatch/internal/jobs"
	"sitewatch/internal/report"
	"sitewatch/internal/sign"
	"sitewatch/internal/status"
)

// siteIDs: alle bekannten Sites, auch die ohne Meldung (Token je Site und Sites der Agenten), sortiert und ohne Doppelte.
func (s *Server) siteIDs() ([]string, error) {
	tokens, err := auth.Load(s.TokensPath)
	if err != nil {
		return nil, err
	}
	ids := auth.Sites(tokens)
	if s.Agents != nil {
		more, err := s.Agents.SiteIDs()
		if err != nil {
			return nil, err
		}
		ids = append(ids, more...)
	}
	sort.Strings(ids)
	out := ids[:0]
	for i, id := range ids {
		if i == 0 || id != ids[i-1] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *Server) siteIDsOfTokens() ([]string, error) {
	tokens, err := auth.Load(s.TokensPath)
	return auth.Sites(tokens), err
}

// agentMayReport: das Token gehört zu einem Agenten, in dessen Konfiguration die Site steht.
func (s *Server) agentMayReport(token, siteID string) bool {
	if s.Agents == nil {
		return false
	}
	a, ok, err := s.Agents.ByToken(token)
	if err != nil || !ok {
		return false
	}
	for _, st := range a.Config.Sites {
		if st.ID == siteID {
			return true
		}
	}
	return false
}

// agentConfig liefert dem Agenten seine signierte Konfiguration.
func (s *Server) agentConfig(w http.ResponseWriter, r *http.Request) {
	a, ok, err := s.Agents.ByToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil {
		log.Printf("agents: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "nicht autorisiert", http.StatusUnauthorized)
		return
	}
	now := s.Now()
	env, err := sign.Config(s.SignKey, a.ID, now, a.Config)
	if err != nil {
		log.Printf("agents: signieren: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	s.fetchMu.Lock()
	if s.fetched == nil {
		s.fetched = map[string]time.Time{}
	}
	s.fetched[a.ID] = now
	s.fetchMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(env)
}

// sameOrigin lehnt schreibende Anfragen ab, die nicht von der eigenen Seite kommen (zusätzlich zu SameSite=Lax am Cookie).
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" {
				http.Error(w, "fremde Herkunft", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "fremde Herkunft", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) agentRoutes(ui *http.ServeMux) {
	ui.HandleFunc("GET /agents", s.agentList)
	ui.HandleFunc("POST /agents", s.agentCreate)
	ui.HandleFunc("GET /agents/{id}", s.agentEdit)
	ui.HandleFunc("POST /agents/{id}", s.agentSave)
	ui.HandleFunc("POST /agents/{id}/rotate", s.agentRotate)
	ui.HandleFunc("POST /agents/{id}/delete", s.agentDelete)
}

type agentRow struct {
	ID      string
	Sites   int
	Fetched string
}

func (s *Server) fetchedText(id string) string {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	if t, ok := s.fetched[id]; ok {
		return status.Ago(s.Now().Sub(t))
	}
	return "noch nicht (seit dem Start von Sitewatch)"
}

func (s *Server) agentList(w http.ResponseWriter, r *http.Request) {
	as, err := s.Agents.List()
	if err != nil {
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	rows := make([]agentRow, len(as))
	for i, a := range as {
		rows[i] = agentRow{a.ID, len(a.Config.Sites), s.fetchedText(a.ID)}
	}
	s.render(w, "agents.html", map[string]any{"Agents": rows, "Error": r.URL.Query().Get("error")})
}

func (s *Server) agentCreate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.FormValue("id"))
	tok, err := s.Agents.Create(id, s.Now())
	if err != nil {
		http.Redirect(w, r, "/agents?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	s.showToken(w, r, id, tok)
}

const dockerWrapperDir = "/usr/share/sitewatch-agent/docker/" // Ziel der Pakete sitewatch-agent, siehe packaging/

// dockerTools liefert für die angekreuzten Programme (Formularfelder docker_<name>) den Wrapper aus dem Paket;
// nicht angekreuzte laufen direkt auf dem Host und brauchen keinen Eintrag.
func dockerTools(r *http.Request) []map[string]string {
	var out []map[string]string
	for _, n := range []string{"composer", "php", "nginx", "mariadb"} {
		if r.FormValue("docker_"+n) != "" {
			out = append(out, map[string]string{"Name": n, "Path": dockerWrapperDir + n + "-in-docker"})
		}
	}
	return out
}

// showToken zeigt das neue Token einmalig mit dem Block für die lokale Datei des Agenten.
func (s *Server) showToken(w http.ResponseWriter, r *http.Request, id, tok string) {
	tools := dockerTools(r)
	base := strings.TrimRight(s.PublicURL, "/")
	if base == "" { // ohne -public-url zählt der Host der Anfrage; X-Forwarded-Proto wird bewusst nicht ausgewertet
		base = "https://" + r.Host
		if strings.HasPrefix(r.Host, "localhost") && r.TLS == nil {
			base = "http://" + r.Host
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	pub := sign.EncodePublicKey(s.SignKey.Public().(ed25519.PublicKey))
	s.render(w, "agent_token.html", map[string]any{"ID": id, "Jobs": s.jobsUI(), "Token": tok, "Sitewatch": base, "PublicKey": pub, "Tools": tools})
}

type siteForm struct {
	N                                                int
	ID, Name, Kind, URL, Path, BackupDir, BackupGlob string
	SiteDir, Ignore                                  string
	Host, NoNginx, NoMariaDB                         bool
	Actions                                          []actionBox // nur bei bestehenden Sites und mit Aufträgen
}

// actionBox: eine Aktion zum Ankreuzen in der Whitelist einer Site; Reported = der Agent hat sie als lokal erlaubt gemeldet.
type actionBox struct {
	Name, Label  string
	On, Reported bool
}

func (s *Server) toForm(i int, st agentcfg.Site, agent string) siteForm {
	f := siteForm{N: i, ID: st.ID, Name: st.Name, Kind: st.Kind, URL: st.URL, Path: st.Path, BackupDir: st.BackupDir,
		BackupGlob: st.BackupGlob, SiteDir: st.Env["SITE_DIR"], Ignore: strings.Join(st.IgnoreUpdates, "\n"), Host: st.Host,
		NoNginx: st.Nginx != nil && !*st.Nginx, NoMariaDB: st.MariaDB != nil && !*st.MariaDB}
	if s.jobsUI() && st.Kind != "host" {
		ann, _ := s.announced(agent)
		for _, act := range jobs.Actions {
			if act.AllowsKind(st.Kind) {
				f.Actions = append(f.Actions, actionBox{act.Name, act.Label, slices.Contains(st.Actions, act.Name), slices.Contains(ann.Actions, act.Name)})
			}
		}
	}
	return f
}

func (s *Server) editPage(w http.ResponseWriter, a agents.Agent, cfg agentcfg.Remote, msg string, code int) {
	forms := make([]siteForm, 0, len(cfg.Sites)+1)
	for i, st := range cfg.Sites {
		forms = append(forms, s.toForm(i, st, a.ID))
	}
	forms = append(forms, siteForm{N: len(forms), Kind: "contao"}) // leere Zeile für eine neue Site
	nonce := newJobID()
	// Nur diese Seite führt ein Skript aus (Nonce): der Knopf „Gemeldete alle wählen“. Ohne JavaScript bleibt er verborgen.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; manifest-src 'self'; script-src 'nonce-"+nonce+"'; base-uri 'none'; form-action 'self'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	preview, _ := json.MarshalIndent(a.Config, "", "  ") // der gespeicherte Stand, nicht die ungespeicherten Eingaben
	s.render(w, "agent.html", map[string]any{"Preview": string(preview), "TTLDays": int(sign.TTL.Hours() / 24), "Now": s.Now(), "Expires": s.Now().Add(sign.TTL), "Agent": a, "Interval": cfg.Interval, "Sites": forms, "Count": len(forms),
		"Error": msg, "Fetched": s.fetchedText(a.ID), "Channel": s.channelText(a.ID), "Nonce": nonce})
}

func (s *Server) agentEdit(w http.ResponseWriter, r *http.Request) {
	a, err := s.Agents.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.editPage(w, a, a.Config, r.URL.Query().Get("error"), http.StatusOK)
}

const maxSiteRows = 50

// parseSites liest die Zeilen s<n>_… des Formulars; Zeilen mit „löschen“ oder ganz ohne ID fallen weg.
func parseSites(r *http.Request) agentcfg.Remote {
	cfg := agentcfg.Remote{Interval: strings.TrimSpace(r.FormValue("interval"))}
	n, _ := strconv.Atoi(r.FormValue("count"))
	for i := 0; i < min(n, maxSiteRows); i++ {
		f := func(k string) string { return strings.TrimSpace(r.FormValue(fmt.Sprintf("s%d_%s", i, k))) }
		if f("delete") != "" || f("id") == "" {
			continue
		}
		st := agentcfg.Site{ID: f("id"), Name: f("name"), Kind: f("kind"), URL: f("url"), Path: f("path"),
			BackupDir: f("backup_dir"), BackupGlob: f("backup_glob"), Host: f("host") != ""}
		for _, act := range jobs.Actions { // in der Reihenfolge der Liste; Unbekanntes fällt weg
			if slices.Contains(r.Form[fmt.Sprintf("s%d_act", i)], act.Name) {
				st.Actions = append(st.Actions, act.Name)
			}
		}
		if d := f("site_dir"); d != "" {
			st.Env = map[string]string{"SITE_DIR": d}
		}
		no := false
		if f("no_nginx") != "" {
			st.Nginx = &no
		}
		if f("no_mariadb") != "" {
			st.MariaDB = &no
		}
		for _, l := range strings.Split(r.FormValue(fmt.Sprintf("s%d_ignore", i)), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				st.IgnoreUpdates = append(st.IgnoreUpdates, l)
			}
		}
		cfg.Sites = append(cfg.Sites, st)
	}
	return cfg
}

func (s *Server) agentSave(w http.ResponseWriter, r *http.Request) {
	a, err := s.Agents.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	// Ein unlesbares oder unvollständiges Formular darf die Konfiguration nicht leeren.
	if err := r.ParseForm(); err != nil || r.PostForm.Get("count") == "" {
		http.Error(w, "ungültiges Formular", http.StatusBadRequest)
		return
	}
	cfg := parseSites(r)
	if !s.jobsUI() { // ohne Aufträge zeigt das Formular keine Auswahl: nichts überschreiben
		for i := range cfg.Sites {
			for _, old := range a.Config.Sites {
				if old.ID == cfg.Sites[i].ID {
					cfg.Sites[i].Actions = old.Actions
				}
			}
		}
	}
	if ids, err := s.siteIDsOfTokens(); err == nil {
		for _, st := range cfg.Sites {
			for _, id := range ids {
				if st.ID == id {
					s.editPage(w, a, cfg, fmt.Sprintf("Site %q hat noch ein eigenes Token; erst mit „sitewatch token -remove %s“ löschen", id, id), http.StatusUnprocessableEntity)
					return
				}
			}
		}
	}
	if err := s.Agents.Save(a.ID, cfg, s.Now()); err != nil {
		s.editPage(w, a, cfg, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	log.Printf("agents: Konfiguration von %q gespeichert (%d Sites)", a.ID, len(cfg.Sites))
	http.Redirect(w, r, "/agents/"+a.ID, http.StatusSeeOther)
}

func (s *Server) agentRotate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok, err := s.Agents.Rotate(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	log.Printf("agents: Token von %q erneuert", id)
	s.showToken(w, r, id, tok)
}

func (s *Server) agentDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.FormValue("confirm") == "" || !report.ValidID(id) || s.Agents.Delete(id) != nil {
		http.NotFound(w, r)
		return
	}
	log.Printf("agents: %q gelöscht", id)
	http.Redirect(w, r, "/agents", http.StatusSeeOther)
}
