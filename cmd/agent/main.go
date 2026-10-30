// Der Agent sammelt Zustandsdaten einer Site und pusht sie ausgehend per HTTPS an das Cockpit.
// Auf dem Zielserver muss kein Port geöffnet werden.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"cockpit/internal/agentcfg"
	"cockpit/internal/collect"
)

// version wird beim Build per -ldflags "-X main.version=..." gesetzt (scripts/build.sh).
var version = "dev"

// job ist eine Site mit allem, was zum Sammeln und Senden nötig ist.
type job struct {
	cfg          collect.Config
	token, spool string
}

func main() {
	var (
		configFile = flag.String("config", "", "Konfigurationsdatei (JSON) mit mehreren Sites; ersetzt die Einzel-Flags unten")
		bootstrap  = flag.String("bootstrap", "", "lokale Datei (JSON) für die Konfiguration aus dem Cockpit: Cockpit-URL, Agent, Token, öffentlicher Schlüssel; Sites und Intervall holt der Agent beim Cockpit")
		siteID     = flag.String("site-id", "", "Site-ID (a-z, 0-9, -)")
		name       = flag.String("name", "", "Anzeigename")
		kind       = flag.String("kind", "contao", `"contao", "php" oder "host" (nur Servermesswerte)`)
		siteURL    = flag.String("url", "", "öffentliche URL der Site")
		path       = flag.String("path", "", "Projektverzeichnis der Site (bei kind=host optional: Dateisystem für den freien Platz)")
		cockpit    = flag.String("cockpit", "", "Basis-URL des Cockpits, z. B. https://cockpit.example")
		token      = flag.String("token", os.Getenv("COCKPIT_TOKEN"), "Token (oder Umgebungsvariable COCKPIT_TOKEN)")
		composer   = flag.String("composer", "composer", "Pfad zum composer-Binary")
		php        = flag.String("php", "php", "Pfad zum php-Binary")
		backups    = flag.String("backup-dir", "", "Verzeichnis, in dem die Backups liegen (für die Altersprüfung)")
		bkGlob     = flag.String("backup-glob", "*", `Dateimaske für Backups im -backup-dir, z. B. "*.tar.gz"`)
		interval   = flag.Duration("interval", 5*time.Minute, "Abstand zwischen Reports")
		once       = flag.Bool("once", false, "einmal senden und beenden")
		host       = flag.Bool("host", true, "Host-Infos (nginx, MariaDB, Speicherplatz) mit erfassen")
		spool      = flag.String("spool", "", "Verzeichnis zum Zwischenspeichern, wenn das Cockpit nicht erreichbar ist")
		dryRun     = flag.Bool("dry-run", false, "Report nur ausgeben, nichts senden")
		showVer    = flag.Bool("version", false, "Version ausgeben und beenden")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("cockpit-agent", version)
		return
	}
	log.Printf("Cockpit-Agent %s startet", version)

	var jobs []job
	if *configFile != "" || *bootstrap != "" {
		single := map[string]bool{"site-id": true, "name": true, "kind": true, "url": true, "path": true, "cockpit": true, "token": true,
			"composer": true, "php": true, "backup-dir": true, "backup-glob": true, "interval": true, "host": true, "spool": true}
		flag.Visit(func(f *flag.Flag) {
			if single[f.Name] {
				log.Fatalf("-%s gilt nur ohne -config/-bootstrap (steht stattdessen in der Konfigurationsdatei)", f.Name)
			}
		})
	}
	if *configFile != "" && *bootstrap != "" {
		log.Fatal("-config und -bootstrap schließen sich aus")
	}
	if *bootstrap != "" {
		bs, err := agentcfg.LoadBootstrap(*bootstrap)
		if err != nil {
			log.Fatal(err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		runRemote(ctx, &http.Client{Timeout: 20 * time.Second}, bs, *once, *dryRun)
		return
	}
	if *configFile != "" {
		c, err := agentcfg.Load(*configFile)
		if err != nil {
			log.Fatal(err)
		}
		*cockpit, *interval = c.Cockpit, c.Interval
		jobs = jobsFrom(c)
	} else {
		if *siteID == "" || (*path == "" && *kind != "host") || (!*dryRun && (*cockpit == "" || *token == "")) {
			fmt.Fprintln(os.Stderr, "benötigt: -config oder -site-id, -path (außer kind=host) sowie -cockpit und -token (außer bei -dry-run)")
			flag.Usage()
			os.Exit(2)
		}
		if !*dryRun {
			if err := agentcfg.CheckCockpitURL(*cockpit); err != nil {
				log.Fatal(err)
			}
		}
		// Relative Programmpfade vor dem Wechsel ins Site-Verzeichnis auflösen.
		jobs = []job{{token: *token, spool: *spool, cfg: collect.Config{
			SiteID: *siteID, Name: *name, Kind: *kind, URL: *siteURL, Path: *path,
			Composer: absIfPath(*composer), PHP: absIfPath(*php), AgentVersion: version,
			Host: *host, BackupDir: *backups, BackupGlob: *bkGlob,
		}}}
	}
	client := &http.Client{Timeout: 20 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cycle := func() {
		// shortcut: Sites laufen nacheinander (bis 5 Minuten je Site); eine hängende Site verzögert die übrigen, bei Bedarf parallelisieren.
		for _, j := range jobs {
			if ctx.Err() != nil {
				return
			}
			runJob(ctx, client, *cockpit, j, *dryRun)
		}
	}
	cycle()
	if *once {
		return
	}
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cycle()
		}
	}
}

// jobsFrom macht aus der Konfiguration die Aufträge je Site.
func jobsFrom(c agentcfg.Config) []job {
	var jobs []job
	for _, s := range c.Sites {
		// Der Host-Eintrag hat keine Container (kein SITE_DIR): dort wird auf dem Host gesucht.
		ng, db := absIfPath(c.Nginx), absIfPath(c.MariaDB)
		if s.Kind == "host" {
			ng, db = "", ""
		}
		j := job{token: s.Token, cfg: collect.Config{
			SiteID: s.ID, Name: s.Name, Kind: s.Kind, URL: s.URL, Path: s.Path,
			Composer: absIfPath(c.Composer), PHP: absIfPath(c.PHP), AgentVersion: version,
			Nginx: ng, MariaDB: db, NoNginx: s.Nginx != nil && !*s.Nginx, NoMariaDB: s.MariaDB != nil && !*s.MariaDB,
			Host: s.Host, BackupDir: s.BackupDir, BackupGlob: s.BackupGlob, Env: s.EnvList(), IgnoreUpdates: s.IgnoreUpdates,
		}}
		if c.Spool != "" {
			j.spool = filepath.Join(c.Spool, s.ID)
		}
		jobs = append(jobs, j)
	}
	return jobs
}

// runJob sammelt und sendet einen Report. Fehler einer Site betreffen die anderen nicht.
func runJob(ctx context.Context, client *http.Client, cockpit string, j job, dryRun bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	rep, err := collect.Collect(ctx, j.cfg)
	if err != nil {
		log.Printf("%s: Sammeln fehlgeschlagen: %v", j.cfg.SiteID, err)
		return
	}
	body, _ := json.MarshalIndent(rep, "", "  ")
	if dryRun {
		fmt.Println(string(body))
		return
	}
	if j.spool != "" {
		flushSpool(ctx, client, cockpit, j.token, j.spool)
	}
	if err := push(ctx, client, cockpit, j.token, body); err != nil {
		log.Printf("%s: Senden fehlgeschlagen: %v", rep.Site.ID, err)
		if j.spool != "" {
			if serr := saveSpool(j.spool, rep.CollectedAt, body); serr != nil {
				log.Printf("%s: Zwischenspeichern fehlgeschlagen: %v", rep.Site.ID, serr)
			} else {
				log.Printf("%s: Report zwischengespeichert, wird beim nächsten Lauf nachgesendet", rep.Site.ID)
			}
		}
		return
	}
	log.Printf("Report gesendet (%s, %d Updates, %d Hinweise, %d Fehlergruppen)",
		rep.Site.ID, len(rep.Site.Outdated), len(rep.Site.Advisories), len(rep.Errors))
}

func absIfPath(p string) string {
	if strings.ContainsRune(p, filepath.Separator) {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	return p
}

func push(ctx context.Context, c *http.Client, base, token string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/api/v1/reports", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("Cockpit antwortet %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

const maxSpool = 100

func saveSpool(dir string, at time.Time, body []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	for len(files) >= maxSpool { // älteste verwerfen
		_ = os.Remove(files[0])
		files = files[1:]
	}
	name := fmt.Sprintf("%s.json", at.UTC().Format("20060102T150405.000000000Z"))
	return os.WriteFile(filepath.Join(dir, name), body, 0o600)
}

func flushSpool(ctx context.Context, c *http.Client, base, token, dir string) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if err := push(ctx, c, base, token, body); err != nil {
			log.Printf("Nachsenden gestoppt: %v", err)
			return
		}
		_ = os.Remove(f)
	}
}
