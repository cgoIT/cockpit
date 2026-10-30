// Package status bewertet einen Report: OK, Warnung oder Kritisch, jeweils mit Gründen.
package status

import (
	"fmt"
	"strings"
	"time"

	"sitewatch/internal/report"
	"sitewatch/internal/store"
)

type Level int

const (
	OK Level = iota
	Warn
	Crit
)

func (l Level) String() string {
	switch l {
	case Warn:
		return "warn"
	case Crit:
		return "crit"
	}
	return "ok"
}

// Config enthält die Schwellwerte.
type Config struct {
	StaleAfter time.Duration   // keine Meldung seit dieser Zeit = kritisch
	ErrorWarn  int             // ab so vielen Fehlern im Fenster = Warnung
	EOLWarn    time.Duration   // Warnung, wenn das PHP-Support-Ende näher als das ist
	TLSWarn    time.Duration   // Warnung, wenn das Zertifikat in weniger als dieser Zeit abläuft
	BackupMax  time.Duration   // Warnung, wenn das letzte Backup älter ist
	DiskWarn   int             // Warnung, wenn weniger als so viel Prozent Speicher frei sind
	DiskCrit   int             // kritisch, wenn weniger als so viel Prozent frei sind
	CronWarn   time.Duration   // Warnung, wenn der letzte Contao-Cron-Lauf länger her ist
	CronCrit   time.Duration   // kritisch, wenn er noch länger her ist
	MaintWarn  time.Duration   // Warnung, wenn der Wartungsmodus so lange an ist
	MaintCrit  time.Duration   // kritisch, wenn er noch länger an ist (vergessen)
	Retention  store.Retention // Aufbewahrung des Verlaufs und der Tagesstände (siehe store)
	EOL        EOLSource       // optional: aktuelle Support-Enden (z. B. endoflife.date); nil = nur die eingebaute PHP-Tabelle
}

func DefaultConfig() Config {
	return Config{StaleAfter: 15 * time.Minute, ErrorWarn: 10, EOLWarn: 90 * 24 * time.Hour,
		TLSWarn: 14 * 24 * time.Hour, BackupMax: 36 * time.Hour, DiskWarn: 15, DiskCrit: 5, CronWarn: 2 * time.Hour, CronCrit: 4 * time.Hour, MaintWarn: 2 * time.Hour, MaintCrit: 8 * time.Hour,
		Retention: store.DefaultRetention()}
}

// phpEOL: Ende der Sicherheitsunterstützung je PHP-Minor-Version.
// Stand Oktober 2026, bitte bei neuen PHP-Versionen pflegen.
var phpEOL = map[string]time.Time{
	"8.0": date(2023, 11, 26),
	"8.1": date(2025, 12, 31),
	"8.2": date(2026, 12, 31),
	"8.3": date(2027, 12, 31),
	"8.4": date(2028, 12, 31),
	"8.5": date(2029, 12, 31),
}

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 23, 59, 59, 0, time.UTC) }

// Result ist das Ergebnis der Bewertung.
type Result struct {
	Level   Level
	Reasons []string
}

// Evaluate bewertet den letzten Report einer Site. receivedAt ist der Eingangszeitpunkt in Sitewatch.
func Evaluate(r *report.Report, receivedAt, now time.Time, cfg Config) Result {
	var res Result
	raise := func(l Level, format string, a ...any) {
		if l > res.Level {
			res.Level = l
		}
		res.Reasons = append(res.Reasons, fmt.Sprintf(format, a...))
	}
	if age := now.Sub(receivedAt); age > cfg.StaleAfter {
		raise(Crit, "Keine Meldung seit %s", Duration(age))
	}
	if n := len(r.Site.Advisories); n > 0 {
		raise(Crit, "%d Sicherheitshinweis(e) von composer audit", n)
	}
	for _, p := range []struct{ product, label, version string }{
		{"php", "PHP", r.Site.PHP}, {"contao", "Contao", r.Site.Contao},
		{"mariadb", "MariaDB", r.Host.MariaDB}, {"nginx", "nginx", r.Host.Nginx},
	} {
		if l, eol := EOLLevel(p.product, p.version, now, cfg); l > OK {
			if eol.Before(now) {
				raise(l, "%s %s hat kein Support mehr (seit %s)", p.label, minor(p.version), eol.Format("02.01.2006"))
			} else {
				raise(l, "%s %s: Support endet am %s", p.label, minor(p.version), eol.Format("02.01.2006"))
			}
		}
	}
	if t := r.Site.TLSExpires; t != nil {
		switch l := TLSLevel(*t, now, cfg); {
		case l == Crit:
			raise(Crit, "TLS-Zertifikat ist abgelaufen (seit %s)", t.Format("02.01.2006"))
		case l == Warn:
			raise(Warn, "TLS-Zertifikat läuft am %s ab", t.Format("02.01.2006"))
		}
	}
	if t := r.Site.LastBackup; t != nil && BackupLevel(*t, now, cfg) > OK {
		raise(Warn, "Letztes Backup vor %s", Duration(now.Sub(*t)))
	}
	if p := r.Host.DiskFreePct; p != nil {
		switch DiskLevel(*p, cfg) {
		case Crit:
			raise(Crit, "Nur noch %d %% Speicherplatz frei", *p)
		case Warn:
			raise(Warn, "Nur noch %d %% Speicherplatz frei", *p)
		}
	}
	if t := r.Site.CronLastRun; t != nil {
		if l := CronLevel(*t, now, cfg); l > OK {
			raise(l, "Contao-Cron zuletzt vor %s gelaufen", Duration(now.Sub(*t)))
		}
	}
	if m, t := r.Site.MaintenanceMode, r.Site.MaintenanceSince; m != nil && *m && t != nil {
		if l := MaintenanceLevel(*t, now, cfg); l > OK {
			raise(l, "Wartungsmodus seit %s an", Duration(now.Sub(*t)))
		}
	}
	if n := r.Site.PendingMigrations; n != nil && *n > 0 {
		raise(Crit, "%d offene Migration(en) bzw. Schema-Änderung(en)", *n)
	}
	if n := r.Site.OpenUpdates(); n > 0 {
		raise(Warn, "%d Paket(e) mit verfügbarem Update", n)
	}
	total := 0
	for _, e := range r.Errors {
		total += e.Count
	}
	if total >= cfg.ErrorWarn {
		raise(Warn, "%d Fehler in den letzten 24 Stunden", total)
	}
	if len(r.Site.Problems) > 0 {
		raise(Warn, "Teilweise keine Daten (%d Problem(e) beim Sammeln)", len(r.Site.Problems))
	}
	return res
}

// EOLSource liefert das Ende der Sicherheitsunterstützung je Produkt ("php", "contao", "mariadb", "nginx") und Version.
// ok=false: unbekannt. Beispiel: eol.Provider (endoflife.date).
type EOLSource interface {
	EOL(product, version string) (time.Time, bool)
}

// EOLLevel, TLSLevel, BackupLevel und DiskLevel sind die Schwellen für Evaluate und für die Matrix-Ansicht.
// Für PHP gilt ohne Treffer in cfg.EOL die eingebaute Tabelle phpEOL; für die anderen Produkte ist ohne Daten alles OK.
func EOLLevel(product, version string, now time.Time, cfg Config) (Level, time.Time) {
	var eol time.Time
	var ok bool
	if cfg.EOL != nil {
		eol, ok = cfg.EOL.EOL(product, version)
	}
	if !ok && product == "php" {
		eol, ok = phpEOL[minor(version)]
	}
	if ok && eol.Sub(now) < cfg.EOLWarn {
		return Warn, eol
	}
	return OK, eol
}

func TLSLevel(expires, now time.Time, cfg Config) Level {
	switch {
	case expires.Before(now):
		return Crit
	case expires.Sub(now) < cfg.TLSWarn:
		return Warn
	}
	return OK
}

func BackupLevel(last, now time.Time, cfg Config) Level {
	if now.Sub(last) > cfg.BackupMax {
		return Warn
	}
	return OK
}

func MaintenanceLevel(since, now time.Time, cfg Config) Level {
	switch age := now.Sub(since); {
	case age > cfg.MaintCrit:
		return Crit
	case age > cfg.MaintWarn:
		return Warn
	}
	return OK
}

func CronLevel(last, now time.Time, cfg Config) Level {
	switch age := now.Sub(last); {
	case age > cfg.CronCrit:
		return Crit
	case age > cfg.CronWarn:
		return Warn
	}
	return OK
}

// shortcut: keine Hysterese, pendelt der Wert um die Schwelle, wechseln Status und Alarm; bei Bedarf eine Toleranz ergänzen.
func DiskLevel(freePct int, cfg Config) Level {
	switch {
	case freePct < cfg.DiskCrit:
		return Crit
	case freePct < cfg.DiskWarn:
		return Warn
	}
	return OK
}

func minor(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// Duration formatiert eine Dauer knapp auf Deutsch.
func Duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "weniger als 1 Min."
	case d < time.Hour:
		return fmt.Sprintf("%d Min.", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d Std.", int(d.Hours()))
	}
	return fmt.Sprintf("%d Tagen", int(d.Hours()/24))
}

// Ago formuliert "vor 5 Min." bzw. "gerade eben".
func Ago(d time.Duration) string {
	if d < time.Minute {
		return "gerade eben"
	}
	return "vor " + Duration(d)
}
