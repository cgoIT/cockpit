// Package report definiert das Report-Format, das Agenten an das Cockpit senden.
// Agent und Cockpit nutzen dieselben Typen, damit das Schema nur einmal existiert.
package report

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// SchemaVersion ist die aktuelle Version des Report-Formats.
const SchemaVersion = 1

// Report ist ein vollständiger Zustandsbericht für genau eine Site.
type Report struct {
	Schema      int        `json:"schema"`
	Agent       Agent      `json:"agent"`
	Host        Host       `json:"host"`
	Site        Site       `json:"site"`
	Errors      []LogError `json:"errors"`
	CollectedAt time.Time  `json:"collected_at"`
}

// Agent beschreibt, wer den Report erzeugt hat (Go-Host-Agent oder PHP-Bundle).
type Agent struct {
	Type    string `json:"type"`
	Version string `json:"version"`
}

// Host enthält Server-Informationen. Alle Felder sind optional.
type Host struct {
	Name        string `json:"name"`
	Nginx       string `json:"nginx,omitempty"`
	MariaDB     string `json:"mariadb,omitempty"`
	DiskFreePct *int   `json:"disk_free_pct,omitempty"`
}

// Site beschreibt die überwachte Webseite.
type Site struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Kind            string `json:"kind"` // "contao", "php" oder "host"
	URL             string `json:"url,omitempty"`
	PHP             string `json:"php,omitempty"`
	Contao          string `json:"contao,omitempty"`
	MaintenanceMode *bool  `json:"maintenance_mode,omitempty"` // nil = unbekannt
	// MaintenanceSince ist der Zeitpunkt, seit dem der Wartungsmodus an ist (Änderungsdatum der Wartungsdatei).
	MaintenanceSince  *time.Time `json:"maintenance_since,omitempty"`
	TLSExpires        *time.Time `json:"tls_expires,omitempty"`        // nil = unbekannt
	LastBackup        *time.Time `json:"last_backup,omitempty"`        // nil = unbekannt
	BackupCount       *int       `json:"backup_count,omitempty"`       // Dateien, die der Backup-Maske entsprechen; nil = nicht geprüft
	CronLastRun       *time.Time `json:"cron_last_run,omitempty"`      // letzter Lauf eines Contao-Cron-Jobs; nil = unbekannt
	PendingMigrations *int       `json:"pending_migrations,omitempty"` // offene Migrationen und Schema-Befehle; nil = unbekannt
	PackagesTotal     int        `json:"packages_total"`
	Outdated          []Package  `json:"outdated"`
	Advisories        []Advisory `json:"advisories"`
	// Problems listet Dinge, die der Agent nicht ermitteln konnte.
	Problems []string `json:"problems,omitempty"`
}

// Package ist ein Composer-Paket mit verfügbarem Update.
type Package struct {
	Name      string `json:"name"`
	Installed string `json:"installed"`
	Latest    string `json:"latest"`
	// Status stammt von "composer outdated": "semver-safe-update" (innerhalb der
	// Constraints) oder "update-possible" (neue Hauptversion außerhalb).
	Status    string `json:"status"`
	Abandoned bool   `json:"abandoned,omitempty"`
	// Ignored: per Regel in der Agent-Konfiguration ausgeschlossen (z. B. keine neue Hauptversion); zählt nicht als offenes Update.
	Ignored bool `json:"ignored,omitempty"`
}

// OpenUpdates zählt die Pakete mit verfügbarem Update, die nicht ausgeschlossen sind.
func (s Site) OpenUpdates() int {
	n := 0
	for _, p := range s.Outdated {
		if !p.Ignored {
			n++
		}
	}
	return n
}

// Advisory ist ein von "composer audit" gemeldeter Sicherheitshinweis.
type Advisory struct {
	Package string `json:"package"`
	Title   string `json:"title"`
	CVE     string `json:"cve,omitempty"`
	Link    string `json:"link,omitempty"`
}

// LogError ist eine gruppierte Fehlermeldung aus dem Log.
type LogError struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Count   int       `json:"count"`
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidID prüft eine Site-ID. Sie wird auch als Dateiname verwendet.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Validate prüft einen empfangenen Report auf Plausibilität und Größenlimits.
func (r *Report) Validate(now time.Time) error {
	if r.Schema != SchemaVersion {
		return fmt.Errorf("unbekannte Schema-Version %d", r.Schema)
	}
	if !ValidID(r.Site.ID) {
		return errors.New("ungültige site.id (erlaubt: a-z, 0-9, -)")
	}
	if k := r.Site.Kind; k != "contao" && k != "php" && k != "host" {
		return errors.New(`site.kind muss "contao", "php" oder "host" sein`)
	}
	if r.CollectedAt.IsZero() {
		return errors.New("collected_at fehlt")
	}
	if r.CollectedAt.After(now.Add(5*time.Minute)) || r.CollectedAt.Before(now.Add(-7*24*time.Hour)) {
		return errors.New("collected_at liegt außerhalb des erlaubten Zeitfensters")
	}
	if n := r.Site.BackupCount; n != nil && *n < 0 {
		return errors.New("backup_count darf nicht negativ sein")
	}
	if n := r.Site.PendingMigrations; n != nil && *n < 0 {
		return errors.New("pending_migrations darf nicht negativ sein")
	}
	if t := r.Site.MaintenanceSince; t != nil && t.After(now.Add(5*time.Minute)) {
		return errors.New("maintenance_since liegt in der Zukunft")
	}
	if t := r.Site.CronLastRun; t != nil && t.After(now.Add(5*time.Minute)) {
		return errors.New("cron_last_run liegt in der Zukunft")
	}
	if t := r.Site.LastBackup; t != nil && t.After(now.Add(5*time.Minute)) {
		return errors.New("last_backup liegt in der Zukunft")
	}
	if len(r.Errors) > 100 || len(r.Site.Outdated) > 1000 || len(r.Site.Advisories) > 200 || len(r.Site.Problems) > 50 {
		return errors.New("zu viele Einträge im Report")
	}
	for _, e := range r.Errors {
		if len(e.Message) > 2000 {
			return errors.New("Fehlermeldung zu lang")
		}
	}
	return nil
}
