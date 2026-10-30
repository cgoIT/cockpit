package collect

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sitewatch/internal/report"
)

const consoleBin = "vendor/bin/contao-console"

// ConsoleBin ist der Pfad der Contao-Konsole relativ zur Site (auch für Aufträge von Sitewatch).
const ConsoleBin = consoleBin

// collectContaoConsole fragt über die Contao-Konsole offene Migrationen und den letzten Cron-Lauf ab.
// Beides startet den Kernel und braucht deshalb ein erreichbares PHP samt Datenbank; was fehlschlägt, landet in Problems.
func collectContaoConsole(ctx context.Context, cfg Config, r *report.Report, now time.Time, problem func(string, ...any)) {
	if _, err := os.Stat(filepath.Join(cfg.Path, consoleBin)); err != nil {
		problem("%s nicht gefunden", consoleBin)
		return
	}
	console := func(args ...string) (string, error) {
		out, stderr, code, err := runEnv(ctx, cfg.Env, cfg.Path, cfg.PHP, append([]string{consoleBin}, args...)...)
		switch {
		case err != nil:
			return "", err
		case code != 0:
			return "", fmt.Errorf("Exit %d: %s", code, firstLine(stderr+out))
		}
		return out, nil
	}

	if out, err := console("contao:migrate", "--dry-run", "--format=ndjson"); err != nil {
		problem("offene Migrationen nicht ermittelbar: %v", err)
	} else if n, err := ParseMigrate(out); err != nil {
		problem("offene Migrationen nicht ermittelbar: %v", err)
	} else {
		r.Site.PendingMigrations = &n
	}

	// Das Alter wird in der Datenbank berechnet, damit nur eine Zahl durch die Ausgabe-Tabelle laufen muss.
	// shortcut: setzt gleiche Zeitzone in PHP und Datenbank voraus (lastRun wird von PHP geschrieben, NOW() kommt von der DB); bei Abweichung Zeitzonen angleichen.
	if out, err := console("dbal:run-sql", "SELECT TIMESTAMPDIFF(SECOND, MAX(lastRun), NOW()) AS age FROM tl_cron_job"); err != nil {
		problem("Cron-Lauf nicht ermittelbar: %v", err)
	} else if age, err := ParseCronAge(out); err != nil {
		problem("Cron-Lauf nicht ermittelbar: %v", err)
	} else {
		t := now.Add(-time.Duration(age) * time.Second)
		r.Site.CronLastRun = &t
	}
}

// ParseMigrate zählt offene Migrationen und Schema-Befehle aus der ndjson-Ausgabe von
// `contao:migrate --dry-run --format=ndjson`. Der Exit-Code sagt dabei nichts über offene Arbeit.
func ParseMigrate(out string) (int, error) {
	pending, seen := 0, false
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue // z. B. Hinweise vor der JSON-Ausgabe
		}
		var m struct {
			Type     string   `json:"type"`
			Message  string   `json:"message"`
			Names    []string `json:"names"`
			Commands []string `json:"commands"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch m.Type {
		case "problem", "error":
			return 0, errors.New(m.Message)
		case "migration-pending":
			pending += len(m.Names)
			seen = true
		case "schema-pending":
			pending += len(m.Commands)
			seen = true
		}
	}
	if !seen {
		return 0, errors.New("keine Migrations-Ausgabe erkannt")
	}
	return pending, sc.Err()
}

var intLine = regexp.MustCompile(`^-?\d+$`)

// ParseCronAge liest das Alter in Sekunden aus der Tabelle von `dbal:run-sql`: genau eine Zeile, die nur eine Zahl enthält.
func ParseCronAge(out string) (int64, error) {
	var vals []int64
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); intLine.MatchString(l) {
			n, _ := strconv.ParseInt(l, 10, 64)
			vals = append(vals, n)
		}
	}
	if len(vals) != 1 {
		return 0, errors.New("kein Cron-Lauf in tl_cron_job gefunden") // leere Tabelle: MAX ergibt NULL
	}
	if vals[0] < 0 {
		return 0, errors.New("Cron-Zeitpunkt liegt in der Zukunft (Zeitzonen von PHP und Datenbank prüfen)")
	}
	return vals[0], nil
}

// maintenanceFile ist die Datei, die `contao:maintenance-mode enable` anlegt (manager-bundle, Contao 4.13+);
// ihr Vorhandensein ist der Zustand. Das ist eine reine Dateiprüfung auf dem Projektpfad, kein PHP nötig.
// shortcut: gilt für den globalen Wartungsmodus mit dem Standardpfad; Wartungsmodus je Website-Root (Datenbank) und
// abweichende Pfade werden nicht erkannt, bei Bedarf `contao:maintenance-mode --format=json` auswerten.
const maintenanceFile = "var/maintenance.html"

func collectMaintenance(cfg Config, r *report.Report, problem func(string, ...any)) {
	fi, err := os.Stat(filepath.Join(cfg.Path, maintenanceFile))
	switch {
	case err == nil:
		on, since := true, fi.ModTime().UTC()
		r.Site.MaintenanceMode, r.Site.MaintenanceSince = &on, &since
	case errors.Is(err, os.ErrNotExist):
		off := false
		r.Site.MaintenanceMode = &off
	default:
		problem("Wartungsmodus nicht prüfbar: %v", err)
	}
}
