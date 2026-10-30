// Package agentcfg liest die Konfigurationsdatei des Agenten (mehrere Sites in einem Prozess).
package agentcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sitewatch/internal/jobs"
	"sitewatch/internal/report"
)

// Site beschreibt eine zu überwachende Site (oder den Host).
type Site struct {
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	Kind       string            `json:"kind"` // contao, php, host
	URL        string            `json:"url,omitempty"`
	Path       string            `json:"path,omitempty"`
	BackupDir  string            `json:"backup_dir,omitempty"`
	BackupGlob string            `json:"backup_glob,omitempty"`
	Host       bool              `json:"host,omitempty"` // Host-Infos zusätzlich erfassen (bei kind=host immer)
	Env        map[string]string `json:"env,omitempty"`  // zusätzliche Umgebung für composer/php, z. B. SITE_DIR
	// IgnoreUpdates schließt Updates aus, z. B. ["contao/*:6.*"] (keine neue Hauptversion) oder ["vendor/paket"] (nie melden).
	IgnoreUpdates []string `json:"ignore_updates,omitempty"`
	// Nginx und MariaDB: nur false erlaubt = die Site hat keinen nginx bzw. keine DB (kein Suchen, kein Problem im Report).
	// Actions: die Aufträge, die für diese Site erlaubt sind (Whitelist; leer = keine). Der Agent führt nur aus, was auch lokal in seiner bootstrap.json steht.
	Actions   []string `json:"actions,omitempty"`
	Nginx     *bool    `json:"nginx,omitempty"`
	MariaDB   *bool    `json:"mariadb,omitempty"`
	Token     string   `json:"token,omitempty"`
	TokenFile string   `json:"token_file,omitempty"`
}

type file struct {
	Sitewatch string `json:"sitewatch"`
	Interval  string `json:"interval"`
	Spool     string `json:"spool"`
	Composer  string `json:"composer"`
	PHP       string `json:"php"`
	Nginx     string `json:"nginx"`   // wie php: Programm für `nginx -v` (nicht für kind=host)
	MariaDB   string `json:"mariadb"` // Programm für `mariadb --version` (nicht für kind=host)
	Sites     []Site `json:"sites"`
}

// Config ist die geprüfte Konfiguration. Die Tokens der Sites sind bereits aufgelöst.
type Config struct {
	Sitewatch      string
	Interval       time.Duration
	Spool          string
	Composer, PHP  string
	Nginx, MariaDB string
	Sites          []Site
}

// MinInterval verhindert, dass ein Tippfehler Sitewatch überflutet.
const MinInterval = 30 * time.Second

// Load liest und prüft die Datei. Sie enthält Tokens und darf deshalb für andere nicht lesbar sein (chmod 600).
func Load(path string) (Config, error) {
	data, err := readPrivate(path)
	if err != nil {
		return Config{}, err
	}
	var f file
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // Tippfehler in Schlüsseln nicht stillschweigend ignorieren
	if err := dec.Decode(&f); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if dec.More() {
		return Config{}, fmt.Errorf("%s: unerwartete Daten nach dem JSON-Dokument", path)
	}
	c := Config{Sitewatch: f.Sitewatch, Spool: f.Spool, Composer: f.Composer, PHP: f.PHP, Nginx: f.Nginx, MariaDB: f.MariaDB, Sites: f.Sites, Interval: 5 * time.Minute}
	if err := CheckSitewatchURL(c.Sitewatch); err != nil {
		return Config{}, err
	}
	if f.Interval != "" {
		if c.Interval, err = time.ParseDuration(f.Interval); err != nil || c.Interval < MinInterval {
			return Config{}, fmt.Errorf("interval %q ungültig (mindestens %s)", f.Interval, MinInterval)
		}
	}
	if len(c.Sites) == 0 {
		return Config{}, errors.New("keine Sites konfiguriert")
	}
	if err := needAbs("spool", c.Spool); err != nil {
		return Config{}, err
	}
	seen, tokens := map[string]bool{}, map[string]string{}
	for i := range c.Sites {
		s := &c.Sites[i]
		if err := checkSite(i, *s); err != nil {
			return Config{}, err
		}
		if seen[s.ID] {
			return Config{}, fmt.Errorf("site %q doppelt", s.ID)
		}
		seen[s.ID] = true
		if s.Token, err = resolveToken(*s); err != nil {
			return Config{}, fmt.Errorf("site %q: %w", s.ID, err)
		}
		if other, dup := tokens[s.Token]; dup {
			return Config{}, fmt.Errorf("site %q und %q haben dasselbe Token", other, s.ID)
		}
		tokens[s.Token] = s.ID
	}
	return c, nil
}

// checkSite prüft alles an einer Site außer dem Token (das gibt es nur in der lokalen Datei).
func checkSite(i int, s Site) error {
	if !report.ValidID(s.ID) {
		return fmt.Errorf("sites[%d]: ungültige id %q", i, s.ID)
	}
	if len(s.Name) > 200 || len(s.URL) > 300 {
		return fmt.Errorf("site %q: name oder url zu lang", s.ID)
	}
	if s.URL != "" {
		if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("site %q: url %q muss mit http:// oder https:// beginnen", s.ID, s.URL)
		}
	}
	if s.Kind != "contao" && s.Kind != "php" && s.Kind != "host" {
		return fmt.Errorf("site %q: kind muss contao, php oder host sein", s.ID)
	}
	if s.Kind != "host" && s.Path == "" {
		return fmt.Errorf("site %q: path fehlt", s.ID)
	}
	for name, p := range map[string]string{"path": s.Path, "backup_dir": s.BackupDir, "token_file": s.TokenFile} {
		if err := needAbs(fmt.Sprintf("site %q: %s", s.ID, name), p); err != nil {
			return err
		}
	}
	for k := range s.Env {
		if k == "" || strings.ContainsRune(k, '=') {
			return fmt.Errorf("site %q: ungültiger env-Name %q", s.ID, k)
		}
	}
	if (s.Nginx != nil && *s.Nginx) || (s.MariaDB != nil && *s.MariaDB) {
		return fmt.Errorf("site %q: nginx/mariadb erlauben nur false (Programme stehen auf oberster Ebene)", s.ID)
	}
	seenAct := map[string]bool{}
	for _, name := range s.Actions {
		act, ok := jobs.Find(name)
		switch {
		case !ok:
			return fmt.Errorf("site %q: actions: %q ist keine bekannte Aktion", s.ID, name)
		case !act.AllowsKind(s.Kind):
			return fmt.Errorf("site %q: actions: %q gilt nicht für Sites der Art %q", s.ID, name, s.Kind)
		case seenAct[name]:
			return fmt.Errorf("site %q: actions: %q doppelt", s.ID, name)
		}
		seenAct[name] = true
	}
	for _, rule := range s.IgnoreUpdates {
		if !validRule(rule) {
			return fmt.Errorf("site %q: ignore_updates %q ungültig", s.ID, rule)
		}
	}
	return nil
}

// EnvList liefert die Umgebung der Site als KEY=wert-Liste in fester Reihenfolge.
func (s Site) EnvList() []string {
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+s.Env[k])
	}
	return out
}

// validRule prüft "paket-glob" bzw. "paket-glob:versions-glob".
func validRule(rule string) bool {
	name, ver, hasVer := strings.Cut(rule, ":")
	_, e1 := path.Match(name, "")
	_, e2 := path.Match(ver, "")
	return name != "" && (!hasVer || ver != "") && e1 == nil && e2 == nil
}

func resolveToken(s Site) (string, error) {
	switch {
	case s.Token != "" && s.TokenFile != "":
		return "", errors.New("token und token_file gleichzeitig")
	case strings.TrimSpace(s.Token) != "":
		return strings.TrimSpace(s.Token), nil
	case s.TokenFile != "":
		b, err := readPrivate(s.TokenFile)
		if err != nil {
			return "", err
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("token_file %s ist leer", s.TokenFile)
	}
	return "", errors.New("token oder token_file fehlt")
}

// readPrivate liest eine Datei mit Zugangsdaten und lehnt sie ab, wenn Gruppe oder Andere sie lesen können
// oder sie keine reguläre Datei ist. Rechte werden am geöffneten Handle geprüft (kein Zeitfenster zwischen Prüfen und Lesen).
func readPrivate(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s ist keine reguläre Datei", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s ist für andere lesbar (Rechte %o), bitte chmod 600", path, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err == nil && len(b) > 1<<20 {
		err = fmt.Errorf("%s ist größer als 1 MiB", path)
	}
	return b, err
}

func needAbs(what, p string) error {
	if p != "" && !filepath.IsAbs(p) {
		return fmt.Errorf("%s muss ein absoluter Pfad sein: %q", what, p)
	}
	return nil
}

// CheckSitewatchURL verlangt https; http ist nur für Loopback erlaubt (lokale Tests), damit kein Token im Klartext reist.
func CheckSitewatchURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("sitewatch-URL %q ungültig", raw)
	}
	switch h := u.Hostname(); {
	case u.Scheme == "https":
	case u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1"):
	default:
		return fmt.Errorf("sitewatch-URL %q muss https sein", raw)
	}
	return nil
}
