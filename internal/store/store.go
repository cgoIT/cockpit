// Package store speichert empfangene Reports in SQLite (modernc.org/sqlite, reines Go, kein CGO).
//
// Je Site und Tag bleibt der letzte vollständige Report erhalten (daily_reports); der jüngste ist zugleich "aktuell".
// Der Verlauf (samples) besteht aus kleinen Zeilen mit den Werten, die sich als Kurve eignen, und wird mit dem Alter verdichtet.
package store

import (
	"time"

	"sitewatch/internal/report"
)

// Entry ist ein gespeicherter Report samt Eingangszeit in Sitewatch.
type Entry struct {
	Report     report.Report `json:"report"`
	ReceivedAt time.Time     `json:"received_at"`
}

// Sample ist ein Verlaufspunkt. Nil-Werte waren beim Sammeln nicht bekannt.
type Sample struct {
	At                time.Time // collected_at des Reports (bei verdichteten Punkten der des letzten Reports im Block)
	Level             int       // status.Level; bei verdichteten Punkten das Schlimmste im Block
	DiskFreePct       *int      // bei verdichteten Punkten das Minimum im Block
	OpenUpdates       int
	Errors            int
	PendingMigrations *int
	CronAgeSec        *int // Alter des letzten Cron-Laufs zum Zeitpunkt des Reports
	BackupAgeSec      *int
	Contao, PHP       string
	MariaDB, Nginx    string
}

// SampleOf bildet den Verlaufspunkt zu einem Report. level ist der Status (status.Level) zum Eingangszeitpunkt.
func SampleOf(e Entry, level int) Sample {
	r := e.Report
	s := Sample{
		At: r.CollectedAt, Level: level, DiskFreePct: r.Host.DiskFreePct, OpenUpdates: r.Site.OpenUpdates(),
		PendingMigrations: r.Site.PendingMigrations,
		Contao:            r.Site.Contao, PHP: r.Site.PHP, MariaDB: r.Host.MariaDB, Nginx: r.Host.Nginx,
	}
	for _, le := range r.Errors {
		s.Errors += le.Count
	}
	age := func(t *time.Time) *int {
		if t == nil {
			return nil
		}
		n := int(r.CollectedAt.Sub(*t).Seconds())
		return &n
	}
	s.CronAgeSec, s.BackupAgeSec = age(r.Site.CronLastRun), age(r.Site.LastBackup)
	return s
}

// Retention legt fest, wie lange was aufbewahrt wird (in Tagen).
type Retention struct {
	FullDays    int // Verlauf: jeder Punkt
	SixHourDays int // Verlauf: danach ein Punkt je 6-Stunden-Block
	MaxDays     int // Verlauf: danach ein Punkt je Tag, ab hier gelöscht
	ReportDays  int // vollständige Tagesstände; der jüngste je Site bleibt immer
}

func DefaultRetention() Retention {
	return Retention{FullDays: 30, SixHourDays: 90, MaxDays: 365, ReportDays: 90}
}

// Store ist die Schnittstelle zum Speicher.
type Store interface {
	Save(e Entry, level int) error
	Latest() ([]Entry, error)
	Get(siteID string) (Entry, bool, error)               // jüngster Report einer Site
	Days(siteID string) ([]string, error)                 // Tage mit Tagesstand, neueste zuerst (YYYY-MM-DD, UTC)
	GetDay(siteID, day string) (Entry, bool, error)       // Tagesstand eines Tages
	History(siteID string, limit int) ([]Sample, error)   // neueste zuerst
	Baselines(since time.Time) (map[string]Sample, error) // je Site der älteste Punkt ab since
}
