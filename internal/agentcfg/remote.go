package agentcfg

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"cockpit/internal/jobs"
	"cockpit/internal/sign"
)

// Remote ist der Teil der Konfiguration, den das Cockpit liefert: Intervall und Sites.
// Programme (php, composer, nginx, mariadb), Token und Zwischenspeicher bleiben lokal im Bootstrap:
// wer das Cockpit (oder dessen Signaturschlüssel) in der Hand hat, soll dem Agenten keine Programme unterschieben können.
type Remote struct {
	Interval string `json:"interval,omitempty"`
	Sites    []Site `json:"sites"`
}

// Validate prüft die Remote-Konfiguration. roots (absolute Verzeichnisse, nur beim Agenten bekannt): alle Pfade müssen darunter liegen;
// nil = keine Prüfung (Cockpit beim Speichern). Token-Felder und Umgebungsvariablen außer SITE_DIR sind aus der Ferne nicht erlaubt.
func (r Remote) Validate(roots []string) error {
	if r.Interval != "" {
		if d, err := time.ParseDuration(r.Interval); err != nil || d < MinInterval {
			return fmt.Errorf("interval %q ungültig (mindestens %s)", r.Interval, MinInterval)
		}
	}
	seen := map[string]bool{}
	for i, s := range r.Sites {
		if err := checkSite(i, s); err != nil {
			return err
		}
		if seen[s.ID] {
			return fmt.Errorf("site %q doppelt", s.ID)
		}
		seen[s.ID] = true
		if s.Token != "" || s.TokenFile != "" {
			return fmt.Errorf("site %q: Token gehört nicht in die Konfiguration des Cockpits", s.ID)
		}
		for k := range s.Env {
			if k != "SITE_DIR" {
				return fmt.Errorf("site %q: env %q nicht erlaubt (nur SITE_DIR)", s.ID, k)
			}
		}
		if roots == nil {
			continue
		}
		for name, p := range map[string]string{"path": s.Path, "backup_dir": s.BackupDir, "env SITE_DIR": s.Env["SITE_DIR"]} {
			if p != "" && !under(p, roots) {
				return fmt.Errorf("site %q: %s %q liegt außerhalb der erlaubten Verzeichnisse", s.ID, name, p)
			}
		}
	}
	return nil
}

// under: p liegt in (oder ist) einem der Verzeichnisse. Der Vergleich geht über saubere Pfade, "/var/www-x" liegt nicht in "/var/www".
func under(p string, roots []string) bool {
	p = filepath.Clean(p)
	for _, r := range roots {
		if rel, err := filepath.Rel(filepath.Clean(r), p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Bootstrap ist die lokale Datei eines Agenten im Fernkonfigurations-Modus.
type Bootstrap struct {
	Cockpit        string
	Agent          string
	Token          string
	PublicKey      ed25519.PublicKey
	State          string   // Verzeichnis für die zwischengespeicherte Konfiguration und den Spool
	Roots          []string // erlaubte Verzeichnisse für Pfade aus der Ferne
	Composer, PHP  string
	Nginx, MariaDB string
	// Actions sind die Aufträge, die dieser Agent ausführen darf (leer = der Auftragskanal ist aus, der Agent fragt nie danach).
	Actions []string
}

type bootstrapFile struct {
	Cockpit   string   `json:"cockpit"`
	Agent     string   `json:"agent"`
	Token     string   `json:"token"`
	TokenFile string   `json:"token_file"`
	PublicKey string   `json:"public_key"`
	State     string   `json:"state"`
	Roots     []string `json:"roots"`
	Composer  string   `json:"composer"`
	PHP       string   `json:"php"`
	Nginx     string   `json:"nginx"`
	MariaDB   string   `json:"mariadb"`
	Actions   []string `json:"actions"`
}

// LoadBootstrap liest und prüft die lokale Datei (chmod 600, enthält das Token).
func LoadBootstrap(path string) (Bootstrap, error) {
	data, err := readPrivate(path)
	if err != nil {
		return Bootstrap{}, err
	}
	var f bootstrapFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Bootstrap{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := CheckCockpitURL(f.Cockpit); err != nil {
		return Bootstrap{}, err
	}
	pub, err := sign.ParsePublicKey(f.PublicKey)
	if err != nil {
		return Bootstrap{}, fmt.Errorf("public_key: %w", err)
	}
	if f.Agent == "" {
		return Bootstrap{}, errors.New("agent fehlt")
	}
	if len(f.Roots) == 0 {
		return Bootstrap{}, errors.New("roots fehlt (erlaubte Verzeichnisse für Pfade aus dem Cockpit, z. B. [\"/var/www\"])")
	}
	for name, p := range map[string]string{"state": f.State} {
		if p == "" || !filepath.IsAbs(p) {
			return Bootstrap{}, fmt.Errorf("%s muss ein absoluter Pfad sein", name)
		}
	}
	for _, r := range f.Roots {
		if !filepath.IsAbs(r) || filepath.Clean(r) == "/" {
			return Bootstrap{}, fmt.Errorf("roots: %q muss ein absolutes Verzeichnis unterhalb von / sein", r)
		}
	}
	for _, a := range f.Actions {
		if _, ok := jobs.Find(a); !ok {
			return Bootstrap{}, fmt.Errorf("actions: %q ist keine bekannte Aktion", a)
		}
	}
	tok, err := resolveToken(Site{Token: f.Token, TokenFile: f.TokenFile})
	if err != nil {
		return Bootstrap{}, err
	}
	return Bootstrap{Cockpit: f.Cockpit, Agent: f.Agent, Token: tok, PublicKey: pub, State: f.State, Roots: f.Roots,
		Composer: f.Composer, PHP: f.PHP, Nginx: f.Nginx, MariaDB: f.MariaDB, Actions: f.Actions}, nil
}

// Config macht aus der Fernkonfiguration die Konfiguration des Agenten; alle Sites melden mit dem Token des Agenten.
func (b Bootstrap) Config(r Remote) Config {
	c := Config{Cockpit: b.Cockpit, Interval: 5 * time.Minute, Spool: filepath.Join(b.State, "spool"),
		Composer: b.Composer, PHP: b.PHP, Nginx: b.Nginx, MariaDB: b.MariaDB, Sites: append([]Site(nil), r.Sites...)}
	if d, err := time.ParseDuration(r.Interval); err == nil && d >= MinInterval {
		c.Interval = d
	}
	for i := range c.Sites {
		c.Sites[i].Token = b.Token
	}
	return c
}
