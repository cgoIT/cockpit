// Package collect sammelt Zustandsdaten einer Site und baut daraus einen Report.
// Alles ist lesend: es wird nichts verändert.
package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"cockpit/internal/report"
)

// Config steuert die Datensammlung für eine Site.
type Config struct {
	SiteID         string
	Name           string
	Kind           string // "contao", "php" oder "host" (nur Servermesswerte, kein PHP/Composer)
	URL            string
	Path           string // Projektverzeichnis der Site (enthält composer.lock, var/logs)
	Composer       string // Pfad zum composer-Binary
	PHP            string // Pfad zum php-Binary
	AgentVersion   string
	Window         time.Duration // Zeitfenster für Fehler aus dem Log
	MaxErrors      int
	BackupDir      string   // optional: Verzeichnis, in das die Backups geschrieben werden
	BackupGlob     string   // Dateimaske für Backups (filepath.Match), leer = alle Dateien
	IgnoreUpdates  []string // Regeln "paket-glob" oder "paket-glob:versions-glob" (z. B. "contao/*:6.*"); Treffer zählen nicht als offene Updates
	Nginx, MariaDB string   // optional: Programme für `nginx -v` / `mariadb --version`, z. B. Docker-Wrapper; leer = auf dem Host suchen
	NoNginx        bool     // Site hat keinen nginx bzw. keine DB: nicht suchen, kein Problem melden
	NoMariaDB      bool
	Host           bool     // freien Speicher erfassen und nginx/MariaDB auf dem Server suchen (konfigurierte Wrapper in Nginx/MariaDB gelten auch ohne)
	Env            []string // zusätzliche Umgebung (KEY=wert) für composer/php, z. B. SITE_DIR für die Docker-Wrapper
	Now            func() time.Time
}

// Collect erzeugt einen Report. Einzelne fehlgeschlagene Quellen führen nicht zum
// Abbruch, sondern landen in Site.Problems.
func Collect(ctx context.Context, cfg Config) (report.Report, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Composer == "" {
		cfg.Composer = "composer"
	}
	if cfg.PHP == "" {
		cfg.PHP = "php"
	}
	if cfg.Window == 0 {
		cfg.Window = 24 * time.Hour
	}
	if cfg.MaxErrors == 0 {
		cfg.MaxErrors = 20
	}
	if cfg.Name == "" {
		cfg.Name = cfg.SiteID
	}
	if cfg.Kind == "host" {
		cfg.Host = true
		if cfg.Path == "" {
			cfg.Path = "/" // Dateisystem, dessen freier Platz gemeldet wird
		}
	}
	if st, err := os.Stat(cfg.Path); err != nil || !st.IsDir() {
		return report.Report{}, fmt.Errorf("Site-Pfad %q nicht lesbar", cfg.Path)
	}

	now := cfg.Now().UTC()
	hostname, _ := os.Hostname()
	r := report.Report{
		Schema:      report.SchemaVersion,
		Agent:       report.Agent{Type: "go-host", Version: cfg.AgentVersion},
		Host:        report.Host{Name: hostname},
		CollectedAt: now,
		Errors:      []report.LogError{},
		Site: report.Site{
			ID: cfg.SiteID, Name: cfg.Name, Kind: cfg.Kind, URL: cfg.URL,
			Outdated: []report.Package{}, Advisories: []report.Advisory{},
		},
	}
	problem := func(format string, a ...any) {
		r.Site.Problems = append(r.Site.Problems, fmt.Sprintf(format, a...))
	}

	// PHP-Version (ein Host-Eintrag hat kein PHP)
	if cfg.Kind != "host" {
		if out, _, code, err := runEnv(ctx, cfg.Env, cfg.Path, cfg.PHP, "-r", "echo PHP_VERSION;"); err != nil || code != 0 {
			problem("PHP-Version nicht ermittelbar")
		} else {
			r.Site.PHP = strings.TrimSpace(out)
		}
	}

	if cfg.Kind == "contao" {
		collectComposer(ctx, cfg, &r, problem)
		collectContaoConsole(ctx, cfg, &r, now, problem)
		collectMaintenance(cfg, &r, problem)
		errs, err := ReadLogErrors(filepath.Join(cfg.Path, "var", "logs"), now, cfg.Window, cfg.MaxErrors)
		if err != nil {
			problem("Log nicht lesbar: %v", err)
		}
		r.Errors = errs
	}

	if u, err := url.Parse(cfg.URL); err == nil && u.Scheme == "https" { // Parse normalisiert das Schema auf Kleinbuchstaben
		if t, verr, err := tlsExpiry(ctx, cfg.URL); err != nil {
			problem("TLS-Zertifikat nicht prüfbar: %v", err)
		} else {
			r.Site.TLSExpires = &t
			if verr != nil {
				problem("TLS-Zertifikat nicht vertrauenswürdig: %v", verr)
			}
		}
	}
	if cfg.BackupDir != "" {
		if t, n, err := newestMod(cfg.BackupDir, cfg.BackupGlob); err != nil {
			problem("Backups nicht prüfbar: %v", err)
		} else {
			r.Site.BackupCount = &n
			if n == 0 {
				problem("Keine Backups gefunden (Maske %q)", cfg.BackupGlob)
			} else {
				r.Site.LastBackup = &t
			}
		}
	}

	collectHost(ctx, cfg, &r, problem)
	return r, nil
}

type lockFile struct {
	Packages []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"packages"`
}

func collectComposer(ctx context.Context, cfg Config, r *report.Report, problem func(string, ...any)) {
	// Installierte Versionen direkt aus composer.lock lesen.
	if data, err := os.ReadFile(filepath.Join(cfg.Path, "composer.lock")); err != nil {
		problem("composer.lock nicht lesbar")
	} else {
		var lock lockFile
		if err := json.Unmarshal(data, &lock); err != nil {
			problem("composer.lock ungültig")
		} else {
			r.Site.PackagesTotal = len(lock.Packages)
			for _, p := range lock.Packages {
				if p.Name == "contao/core-bundle" {
					r.Site.Contao = strings.TrimPrefix(p.Version, "v")
				}
			}
		}
	}

	// Verfügbare Updates (direkte Abhängigkeiten).
	out, stderr, code, err := runEnv(ctx, cfg.Env, cfg.Path, cfg.Composer, "outdated", "--direct", "--format=json", "--no-interaction")
	switch {
	case err != nil:
		problem("composer outdated nicht ausführbar: %v", err)
	case code != 0:
		problem("composer outdated fehlgeschlagen (Exit %d): %s", code, firstLine(stderr))
	default:
		pk, perr := ParseOutdated(out)
		if perr != nil {
			problem("composer outdated: %v", perr)
		}
		MarkIgnored(pk, cfg.IgnoreUpdates)
		r.Site.Outdated = pk
	}

	// Sicherheitshinweise. composer audit beendet sich mit Exit 1, wenn es Treffer gibt.
	out, stderr, code, err = runEnv(ctx, cfg.Env, cfg.Path, cfg.Composer, "audit", "--format=json", "--no-interaction")
	switch {
	case err != nil:
		problem("composer audit nicht ausführbar: %v", err)
	case code > 1:
		problem("composer audit fehlgeschlagen (Exit %d): %s", code, firstLine(stderr))
	default:
		adv, perr := ParseAudit(out)
		if perr != nil {
			problem("composer audit: %v", perr)
		}
		r.Site.Advisories = adv
	}
}

// ParseOutdated liest die JSON-Ausgabe von "composer outdated --format=json".
func ParseOutdated(data string) ([]report.Package, error) {
	var doc struct {
		Installed []struct {
			Name         string          `json:"name"`
			Version      string          `json:"version"`
			Latest       string          `json:"latest"`
			LatestStatus string          `json:"latest-status"`
			Abandoned    json.RawMessage `json:"abandoned"`
		} `json:"installed"`
	}
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		return []report.Package{}, errors.New("ungültige JSON-Ausgabe")
	}
	out := make([]report.Package, 0, len(doc.Installed))
	for _, p := range doc.Installed {
		ab := strings.TrimSpace(string(p.Abandoned))
		out = append(out, report.Package{
			Name: p.Name, Installed: p.Version, Latest: p.Latest, Status: p.LatestStatus,
			Abandoned: ab != "" && ab != "false" && ab != "null",
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// MarkIgnored markiert Pakete, auf die eine Regel passt. Eine Regel ist "paket-glob" (jedes Update des Pakets)
// oder "paket-glob:versions-glob" (nur Updates auf eine passende Version, ein führendes "v" wird ignoriert).
func MarkIgnored(pk []report.Package, rules []string) {
	for i := range pk {
		latest := strings.TrimPrefix(pk[i].Latest, "v")
		for _, rule := range rules {
			name, ver, hasVer := strings.Cut(rule, ":")
			if ok, _ := path.Match(name, pk[i].Name); !ok {
				continue
			}
			if ok, _ := path.Match(ver, latest); !hasVer || ok {
				pk[i].Ignored = true
				break
			}
		}
	}
}

// ParseAudit liest die JSON-Ausgabe von "composer audit --format=json".
func ParseAudit(data string) ([]report.Advisory, error) {
	var doc struct {
		Advisories json.RawMessage `json:"advisories"`
	}
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		return []report.Advisory{}, errors.New("ungültige JSON-Ausgabe")
	}
	out := []report.Advisory{}
	trimmed := strings.TrimSpace(string(doc.Advisories))
	if trimmed == "" || trimmed == "[]" || trimmed == "null" {
		return out, nil // Composer liefert bei "keine Treffer" ein leeres Array
	}
	var byPkg map[string][]struct {
		Title string `json:"title"`
		CVE   string `json:"cve"`
		Link  string `json:"link"`
	}
	if err := json.Unmarshal(doc.Advisories, &byPkg); err != nil {
		return out, errors.New("unerwartetes advisories-Format")
	}
	names := make([]string, 0, len(byPkg))
	for n := range byPkg {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, a := range byPkg[n] {
			out = append(out, report.Advisory{Package: n, Title: a.Title, CVE: a.CVE, Link: a.Link})
		}
	}
	return out, nil
}

var (
	nginxRe   = regexp.MustCompile(`nginx/([0-9][0-9.]*)`)
	mariadbRe = regexp.MustCompile(`(?:Distrib|from) ([0-9][0-9.]*)`)
)

// collectHost ermittelt nginx, MariaDB und freien Speicher. Sind Programme konfiguriert (Docker-Wrapper), gelten sie für die Site
// unabhängig von Host; ohne Konfiguration wird nur bei Host auf dem Server gesucht, und "nicht gefunden" bleibt still.
func collectHost(ctx context.Context, cfg Config, r *report.Report, problem func(string, ...any)) {
	if !cfg.NoNginx && (cfg.Nginx != "" || cfg.Host) {
		if _, se, _, err := runEnv(ctx, cfg.Env, cfg.Path, or(cfg.Nginx, "nginx"), "-v"); err == nil {
			if m := nginxRe.FindStringSubmatch(se); m != nil {
				r.Host.Nginx = m[1]
			}
		}
		if r.Host.Nginx == "" && cfg.Nginx != "" {
			problem("nginx-Version nicht ermittelbar")
		}
	}
	if bins := []string{cfg.MariaDB}; !cfg.NoMariaDB && (cfg.MariaDB != "" || cfg.Host) {
		if cfg.MariaDB == "" {
			bins = []string{"mariadb", "mysql"}
		}
		for _, bin := range bins {
			if so, _, _, err := runEnv(ctx, cfg.Env, cfg.Path, bin, "--version"); err == nil {
				if m := mariadbRe.FindStringSubmatch(so); m != nil {
					r.Host.MariaDB = m[1]
					break
				}
			}
		}
		if r.Host.MariaDB == "" && cfg.MariaDB != "" {
			problem("MariaDB-Version nicht ermittelbar")
		}
	}
	if pct, ok := diskFreePct(cfg.Path); ok && cfg.Host {
		r.Host.DiskFreePct = &pct
	}
}

func or(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

func run(ctx context.Context, dir, name string, args ...string) (stdout, stderr string, code int, err error) {
	return runEnv(ctx, nil, dir, name, args...)
}

// runEnv startet ein Programm mit der Prozessumgebung plus env.
func runEnv(ctx context.Context, env []string, dir, name string, args ...string) (stdout, stderr string, code int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		// Bei explizitem Env setzt Go PWD nicht selbst; die Docker-Wrapper brauchen es.
		cmd.Env = append(os.Environ(), append(env, "PWD="+dir)...)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
		err = nil
	}
	return so.String(), se.String(), code, err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
