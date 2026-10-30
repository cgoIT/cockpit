package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"cockpit/internal/agentcfg"
	"cockpit/internal/collect"
	"cockpit/internal/jobs"
	"cockpit/internal/sign"
)

const (
	pollWait    = 20 * time.Second // so lange hält das Cockpit eine Anfrage offen (siehe web.pollMax)
	flushEvery  = 500 * time.Millisecond
	heartbeat   = 2 * time.Second // auch ohne Ausgabe melden, damit ein Abbruch schnell ankommt
	chunkSize   = 16 << 10
	maxBuffered = 512 << 10 // mehr Ausgabe wird verworfen (das Cockpit speichert ohnehin nur 256 KB)
	perPost     = 100       // Blöcke je Anfrage
)

// current ist die zuletzt geprüfte Konfiguration. Der Auftragskanal führt nur Aufträge für Sites daraus aus.
type current struct {
	mu sync.Mutex
	c  agentcfg.Config
	ok bool
}

func (c *current) set(cfg agentcfg.Config) { c.mu.Lock(); c.c, c.ok = cfg, true; c.mu.Unlock() }
func (c *current) get() (agentcfg.Config, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c, c.ok
}

// jobRunner holt Aufträge beim Cockpit ab (Long-Polling, nur ausgehende Verbindungen) und führt sie aus.
type jobRunner struct {
	bs     agentcfg.Bootstrap
	client *http.Client // Timeout länger als pollWait
	cur    *current
	kick   chan<- string // Site, die nach einem Auftrag sofort neu gemeldet werden soll
	seen   seenFile
	now    func() time.Time
	// refresh holt die Konfiguration frisch, bevor ein Auftrag geprüft wird (sie wird sonst nur einmal je Zyklus geholt, eine eben gespeicherte Freigabe wäre noch unbekannt); nil = nicht.
	refresh func(context.Context)
}

func newJobRunner(bs agentcfg.Bootstrap, cur *current, kick chan<- string) *jobRunner {
	// Keine Umleitungen verfolgen: Eine Antwort des Cockpits mit Umleitung (Login-Seite) ist ein Fehler, kein JSON.
	client := &http.Client{Timeout: pollWait + 15*time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &jobRunner{bs: bs, client: client, cur: cur, kick: kick,
		seen: seenFile{filepath.Join(bs.State, "jobs_seen")}, now: time.Now}
}

func (r *jobRunner) loop(ctx context.Context) {
	log.Printf("Auftragskanal aktiv (erlaubt: %s)", strings.Join(r.bs.Actions, ", "))
	backoff, failing := 5*time.Second, false
	for ctx.Err() == nil {
		start := time.Now()
		envs, err := r.poll(ctx)
		if err != nil {
			if !failing {
				log.Printf("Auftragskanal: %v (wird wiederholt)", err)
			}
			failing = true
			sleep(ctx, backoff)
			if backoff *= 2; backoff > 2*time.Minute {
				backoff = 2 * time.Minute
			}
			continue
		}
		if failing {
			log.Print("Auftragskanal: wieder verbunden")
		}
		backoff, failing = 5*time.Second, false
		for _, e := range envs {
			r.handle(ctx, e)
		}
		if time.Since(start) < time.Second { // eine sofortige Antwort (Zwischenstück, wiederholt gelieferter Auftrag) darf nicht zur Schleife werden
			sleep(ctx, time.Second-time.Since(start))
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (r *jobRunner) url(path string) string { return strings.TrimRight(r.bs.Cockpit, "/") + path }

func (r *jobRunner) poll(ctx context.Context) ([]sign.Envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url(fmt.Sprintf("/api/v1/agent/jobs?wait=%d&actions=%s", int(pollWait.Seconds()), url.QueryEscape(strings.Join(r.bs.Actions, ",")))), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.bs.Token)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	var out struct{ Jobs []sign.Envelope }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

// chunk entspricht store.Chunk; der Agent bindet den Speicher (SQLite) bewusst nicht ein.
type chunk struct {
	Seq  int    `json:"seq"`
	Data string `json:"data"`
}

type event struct {
	State   string  `json:"state,omitempty"`
	Exit    *int    `json:"exit,omitempty"`
	Message string  `json:"message,omitempty"`
	Chunks  []chunk `json:"chunks,omitempty"`
}

// statusError beschreibt eine unerwartete Antwort. Eine Umleitung heißt meist: Das Cockpit kennt den Pfad nicht (ältere Version)
// oder ein Reverse-Proxy lässt ihn nicht durch, und die Anfrage landet beim Login.
func statusError(resp *http.Response) error {
	if resp.StatusCode/100 == 3 {
		return fmt.Errorf("Cockpit leitet um (%d nach %q): läuft dort die neue Version, und lässt der Reverse-Proxy /api/v1/agent/jobs durch?", resp.StatusCode, resp.Header.Get("Location"))
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return fmt.Errorf("Cockpit antwortet %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}

// post meldet Zustand/Ausgabe. Die Antwort sagt, ob der Auftrag abgebrochen werden soll.
func (r *jobRunner) post(ctx context.Context, id string, ev event) (cancel bool, err error) {
	body, _ := json.Marshal(ev)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url("/api/v1/agent/jobs/"+id+"/events"), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+r.bs.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, statusError(resp)
	}
	var out struct{ Cancel bool }
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	return out.Cancel, err
}

// postFinal meldet das Ende mit ein paar Wiederholungen: Geht es verloren, bliebe der Auftrag im Cockpit "läuft".
func (r *jobRunner) postFinal(ctx context.Context, id string, ev event) {
	for i := 0; i < 4; i++ {
		if _, err := r.post(ctx, id, ev); err == nil {
			return
		} else if ctx.Err() != nil {
			return
		} else {
			log.Printf("Auftrag %s: Ende nicht gemeldet: %v", id, err)
		}
		sleep(ctx, 3*time.Second)
	}
}

func (r *jobRunner) reject(ctx context.Context, id, why string) {
	log.Printf("Auftrag %s abgelehnt: %s", id, why)
	if id != "" {
		r.postFinal(ctx, id, event{State: "rejected", Message: why})
	}
}

// handle prüft einen Auftrag und führt ihn aus. Reihenfolge mit Absicht: erst alles prüfen, dann beim Cockpit rückfragen
// ("running", dort kann der Auftrag inzwischen abgebrochen sein), erst danach als gesehen vermerken und starten.
func (r *jobRunner) handle(ctx context.Context, env sign.Envelope) {
	now := r.now()
	p, err := sign.VerifyJob(r.bs.PublicKey, env, r.bs.Agent, now)
	if err != nil {
		var lenient struct{ ID string }
		_ = json.Unmarshal(env.Payload, &lenient) // nur für die Meldung an das Cockpit, vertraut wird dem Inhalt nicht
		r.reject(ctx, lenient.ID, err.Error())
		return
	}
	act, known := jobs.Find(p.Action)
	allowed := slices.Contains(r.bs.Actions, p.Action)
	if r.refresh != nil {
		r.refresh(ctx)
	}
	cfg, haveCfg := r.cur.get()
	var site *agentcfg.Site
	for i := range cfg.Sites {
		if cfg.Sites[i].ID == p.Site {
			site = &cfg.Sites[i]
		}
	}
	seen, serr := r.seen.has(p.ID, now)
	switch {
	case serr != nil:
		r.reject(ctx, p.ID, "Zustand der Aufträge nicht lesbar: "+serr.Error())
		return
	case seen:
		r.reject(ctx, p.ID, "Auftrag wurde schon bearbeitet")
		return
	case !known || !allowed:
		r.reject(ctx, p.ID, fmt.Sprintf("Aktion %q ist lokal nicht erlaubt (actions in der bootstrap.json)", p.Action))
		return
	case !haveCfg || site == nil:
		r.reject(ctx, p.ID, fmt.Sprintf("Site %q gehört nicht zur aktuellen Konfiguration dieses Agenten", p.Site))
		return
	}
	if !slices.Contains(site.Actions, p.Action) {
		r.reject(ctx, p.ID, fmt.Sprintf("Aktion %q ist für die Site %q im Cockpit nicht freigegeben", p.Action, p.Site))
		return
	}
	if !act.AllowsKind(site.Kind) {
		r.reject(ctx, p.ID, fmt.Sprintf("Aktion %q gilt nicht für Sites der Art %q", p.Action, site.Kind))
		return
	}
	prog, args, dir, err := commandFor(act, *site, cfg, r.bs.Roots)
	if err != nil {
		r.reject(ctx, p.ID, err.Error())
		return
	}

	cancel, err := r.post(ctx, p.ID, event{State: "running"})
	if err != nil {
		log.Printf("Auftrag %s: Rückfrage beim Cockpit fehlgeschlagen, nichts ausgeführt: %v", p.ID, err)
		return // das Cockpit liefert ihn erneut aus (wartend bis zum Ablauf, laufend ohne Ausgabe 2 Minuten lang)
	}
	if err := r.seen.add(p.ID, p.ExpiresAt, now); err != nil {
		r.postFinal(ctx, p.ID, event{State: "failed", Message: "Zustand der Aufträge nicht schreibbar, nichts ausgeführt: " + err.Error()})
		return
	}
	if cancel {
		log.Printf("Auftrag %s war schon abgebrochen", p.ID)
		r.postFinal(ctx, p.ID, event{State: "canceled", Message: "vor dem Start abgebrochen"})
		return
	}
	log.Printf("Auftrag %s: %s auf %q", p.ID, p.Action, p.Site)
	r.run(ctx, p, act, *site, prog, args, dir)
	select { // die Site sofort neu melden, damit z. B. der Wartungsmodus nicht bis zum nächsten Zyklus unsichtbar bleibt
	case r.kick <- p.Site:
	default:
	}
}

// commandFor baut den Befehl der Aktion aus der festen Liste. Dem Auftrag selbst entnimmt der Agent nichts außer dem Namen der Aktion.
// Site-Aktionen rufen das Werkzeug des Website-Stacks (<SITE_DIR>/bin/site) im Site-Verzeichnis auf, Console-Aktionen die Contao-Konsole.
func commandFor(act jobs.Action, site agentcfg.Site, cfg agentcfg.Config, roots []string) (prog string, args []string, dir string, err error) {
	if len(act.Site) > 0 {
		dir = site.Env["SITE_DIR"]
		if dir == "" {
			return "", nil, "", errors.New("SITE_DIR fehlt in der Konfiguration der Site (Feld „SITE_DIR“ im Cockpit)")
		}
		// SITE_DIR kommt aus der Konfiguration des Cockpits, das Werkzeug läuft als der Agent-Benutzer (meist root): Es muss zur Site gehören,
		// nach Auflösen von Symlinks noch unter roots liegen und eine normale Datei sein.
		if rel, err := filepath.Rel(dir, site.Path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", nil, "", fmt.Errorf("SITE_DIR %q enthält den Pfad der Site %q nicht", dir, site.Path)
		}
		prog = filepath.Join(dir, "bin", "site")
		real, err := filepath.EvalSymlinks(prog)
		if err != nil {
			return "", nil, "", fmt.Errorf("%s nicht gefunden", prog)
		}
		if fi, err := os.Stat(real); err != nil || !fi.Mode().IsRegular() {
			return "", nil, "", fmt.Errorf("%s ist keine normale Datei", prog)
		}
		if !insideRoots(real, roots) {
			return "", nil, "", fmt.Errorf("%s liegt (nach Auflösen von Verknüpfungen) außerhalb der erlaubten Verzeichnisse", prog)
		}
		// shortcut: geprüft wird der aufgelöste Pfad, gestartet der nicht aufgelöste (bin/site bestimmt SITE_DIR aus seinem eigenen Pfad, ein Symlink-Ziel würde das ändern);
		// wer bin/ oder bin/site ändern darf, könnte dazwischen tauschen. Besitzer/Rechte werden nicht geprüft; bei Bedarf root als Besitzer und kein Schreibrecht für andere verlangen.
		return prog, act.Site, dir, nil
	}
	if _, err := os.Stat(filepath.Join(site.Path, collect.ConsoleBin)); err != nil {
		return "", nil, "", errors.New(collect.ConsoleBin + " nicht gefunden")
	}
	php := cfg.PHP
	if php == "" {
		php = "php"
	}
	return absIfPath(php), append([]string{collect.ConsoleBin}, act.Console...), site.Path, nil
}

// insideRoots: path liegt unter einem der erlaubten Verzeichnisse (beide mit aufgelösten Verknüpfungen). Ohne roots nie.
func insideRoots(path string, roots []string) bool {
	for _, r := range roots {
		if rr, err := filepath.EvalSymlinks(r); err == nil {
			r = rr
		}
		if rel, err := filepath.Rel(r, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// run startet die Aktion und streamt die Ausgabe ans Cockpit.
func (r *jobRunner) run(ctx context.Context, p sign.JobPayload, act jobs.Action, site agentcfg.Site, prog string, args []string, dir string) {
	runCtx, stopRun := context.WithTimeout(ctx, act.Limit())
	defer stopRun()
	cmd := exec.CommandContext(runCtx, prog, args...)
	cmd.Dir = dir
	// Wie beim Sammeln: die Docker-Wrapper und das site-Werkzeug brauchen SITE_DIR und PWD. Ohne Terminal keine Farben.
	cmd.Env = append(os.Environ(), append(site.EnvList(), "PWD="+dir, "NO_COLOR=1", "TERM=dumb")...)
	cmd.WaitDelay = 3 * time.Second
	// shortcut: beendet wird nur der gestartete Prozess (bei den Docker-Wrappern der Wrapper, nicht zwingend der Befehl im Container); bei Bedarf Prozessgruppe beenden.
	out := &outBuf{}
	cmd.Stdout, cmd.Stderr = out, out // derselbe Schreiber: die Reihenfolge von stdout und stderr bleibt erhalten

	var canceled bool
	var mu sync.Mutex
	done := make(chan struct{})
	flushed := make(chan struct{})
	go func() { // Ausgabe und Lebenszeichen ans Cockpit; die Antwort kann den Abbruch verlangen
		defer close(flushed)
		t := time.NewTicker(flushEvery)
		defer t.Stop()
		last := time.Now()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			chunks := out.pending(perPost)
			if len(chunks) == 0 && time.Since(last) < heartbeat {
				continue
			}
			c, err := r.post(runCtx, p.ID, event{Chunks: chunks})
			if err != nil {
				continue // die Blöcke bleiben vorgemerkt und gehen mit der nächsten Meldung mit
			}
			last = time.Now()
			out.sent(chunks)
			if c {
				mu.Lock()
				canceled = true
				mu.Unlock()
				stopRun()
			}
		}
	}()

	runErr := cmd.Run()
	close(done)
	<-flushed

	ev := event{}
	exit := 0
	var ee *exec.ExitError
	mu.Lock()
	wasCanceled := canceled
	mu.Unlock()
	switch {
	case wasCanceled:
		ev.State, ev.Message = "canceled", "abgebrochen"
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		ev.State, ev.Message = "failed", fmt.Sprintf("Zeitlimit von %s überschritten", act.Limit())
	case runErr == nil:
		ev.State, ev.Exit = "ok", &exit
	case errors.As(runErr, &ee):
		exit = ee.ExitCode()
		ev.State, ev.Exit, ev.Message = "failed", &exit, fmt.Sprintf("Exit-Code %d", exit)
	default:
		ev.State, ev.Message = "failed", "Start fehlgeschlagen: "+runErr.Error()
	}
	// Alles Restliche in Etappen schicken, das Ende zuletzt.
	for {
		rest := out.pending(perPost)
		if len(rest) == 0 {
			break
		}
		if _, err := r.post(ctx, p.ID, event{Chunks: rest}); err != nil {
			break
		}
		out.sent(rest)
	}
	ev.Chunks = out.pending(perPost)
	r.postFinal(ctx, p.ID, ev)
	log.Printf("Auftrag %s beendet: %s", p.ID, ev.State)
}

const (
	escNone  = iota
	escStart // nach ESC
	escCSI   // in ESC [ ...
	escOSC   // in ESC ] ...
)

// outBuf sammelt die Ausgabe des Prozesses und teilt sie in nummerierte Blöcke. Vorgemerkte Blöcke bleiben, bis das Cockpit sie bestätigt hat.
type outBuf struct {
	mu      sync.Mutex
	buf     []byte
	dropped bool
	esc     int // Zustand des Filters für Steuersequenzen (esc*)
	total   int
	seq     int
	unsent  []chunk
}

// Write entfernt dabei Steuersequenzen für Farben und Cursor (ESC [ ... Endbyte, ESC + ein Zeichen). Zustand bleibt über Aufrufe
// erhalten, weil eine Sequenz auf zwei Schreibvorgänge verteilt sein kann.
func (o *outBuf) Write(raw []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := make([]byte, 0, len(raw))
	for _, b := range raw {
		switch o.esc {
		case escStart: // nach ESC
			switch b {
			case '[':
				o.esc = escCSI
			case ']':
				o.esc = escOSC
			case 0x1b: // ESC ESC [: die zweite zählt als Anfang
			default: // ESC + ein Zeichen (z. B. ESC c)
				o.esc = escNone
			}
		case escCSI: // Parameter 0x20-0x3f, Endbyte 0x40-0x7e; alles andere bricht ab (unvollständige Sequenz), das Byte gilt normal
			switch {
			case b >= 0x40 && b <= 0x7e:
				o.esc = escNone
			case b >= 0x20 && b <= 0x3f:
			case b == 0x1b:
				o.esc = escStart
			default:
				o.esc = escNone
				p = append(p, b)
			}
		case escOSC: // bis BEL oder ESC \ ; ein Zeilenumbruch bricht ab
			switch b {
			case 0x07:
				o.esc = escNone
			case 0x1b:
				o.esc = escStart
			case '\n':
				o.esc = escNone
				p = append(p, b)
			}
		default:
			if b == 0x1b {
				o.esc = escStart
			} else {
				p = append(p, b)
			}
		}
	}
	if len(p) == 0 {
		return len(raw), nil
	}
	if o.total+len(p) > maxBuffered {
		if !o.dropped {
			o.dropped = true
			o.buf = append(o.buf, "\n[weitere Ausgabe verworfen]\n"...)
		}
		return len(raw), nil // den Prozess nicht ausbremsen
	}
	o.total += len(p)
	o.buf = append(o.buf, p...)
	return len(raw), nil
}

// pending liefert bis zu max noch nicht bestätigte Blöcke (neue werden aus dem Puffer gebildet).
func (o *outBuf) pending(max int) []chunk {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(o.buf) > 0 {
		n := len(o.buf)
		if n > chunkSize {
			n = chunkSize
		}
		o.seq++
		o.unsent = append(o.unsent, chunk{Seq: o.seq, Data: strings.ToValidUTF8(string(o.buf[:n]), "�")})
		o.buf = o.buf[n:]
	}
	if len(o.unsent) > max {
		return append([]chunk(nil), o.unsent[:max]...)
	}
	return append([]chunk(nil), o.unsent...)
}

func (o *outBuf) sent(c []chunk) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(c) > 0 && len(c) <= len(o.unsent) {
		o.unsent = o.unsent[len(c):]
	}
}

// seenFile merkt sich die IDs bearbeiteter Aufträge bis kurz nach deren Ablauf (danach lehnt schon die Signaturprüfung sie ab).
type seenFile struct{ path string }

func (s seenFile) read(now time.Time) (map[string]time.Time, error) {
	m := map[string]time.Time{}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		id, ts, ok := strings.Cut(strings.TrimSpace(line), " ")
		if t, err := time.Parse(time.RFC3339, ts); ok && err == nil && now.Before(t.Add(time.Hour)) {
			m[id] = t
		}
	}
	return m, nil
}

func (s seenFile) has(id string, now time.Time) (bool, error) {
	m, err := s.read(now)
	_, ok := m[id]
	return ok, err
}

func (s seenFile) add(id string, expires, now time.Time) error {
	m, err := s.read(now)
	if err != nil {
		return err
	}
	m[id] = expires
	var b strings.Builder
	for k, t := range m {
		fmt.Fprintf(&b, "%s %s\n", k, t.UTC().Format(time.RFC3339))
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
