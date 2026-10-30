package collect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sitewatch/internal/report"
)

func TestParseMigrate(t *testing.T) {
	// Ausgabe einer echten Contao-5-Installation ohne offene Arbeit
	none := `{"type":"migration-pending","names":[],"hash":"4f53"}` + "\n" + `{"type":"schema-pending","commands":[],"hash":"4f53"}`
	if n, err := ParseMigrate(none); err != nil || n != 0 {
		t.Errorf("nichts offen: %d %v", n, err)
	}
	some := "Hinweis vor der Ausgabe\n" + `{"type":"migration-pending","names":["a","b"],"hash":"x"}` + "\n" + `{"type":"schema-pending","commands":["ALTER TABLE x"],"hash":"y"}`
	if n, err := ParseMigrate(some); err != nil || n != 3 {
		t.Errorf("offen: %d %v", n, err)
	}
	for name, in := range map[string]string{
		"problem": `{"type":"problem","message":"DB-Version"}`,
		"leer":    "",
		"Text":    "All migrations completed.",
	} {
		if _, err := ParseMigrate(in); err == nil {
			t.Errorf("%s: kein Fehler", name)
		}
	}
}

func TestParseCronAge(t *testing.T) {
	table := " ----- \n  age\n ----- \n  125\n ----- \n"
	if n, err := ParseCronAge(table); err != nil || n != 125 {
		t.Errorf("%d %v", n, err)
	}
	for name, in := range map[string]string{
		"NULL":    " ----- \n  age\n ----- \n  NULL\n ----- \n",
		"negativ": "  age\n  -30\n",
		"zwei":    "1\n2\n",
	} {
		if _, err := ParseCronAge(in); err == nil {
			t.Errorf("%s: kein Fehler", name)
		}
	}
}

func TestCollectContaoConsole(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "vendor/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, consoleBin), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	php := filepath.Join(t.TempDir(), "php") // Stub: $1 = Konsole, $2 = Befehl
	script := "#!/bin/sh\ncase \"$2\" in\ncontao:migrate) echo '{\"type\":\"migration-pending\",\"names\":[\"m\"],\"hash\":\"h\"}';;\ndbal:run-sql) printf ' ---\\n  age\\n ---\\n  300\\n ---\\n';;\nesac\n"
	if err := os.WriteFile(php, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	var r report.Report
	var problems []string
	collectContaoConsole(context.Background(), Config{Path: dir, PHP: php}, &r, now, func(f string, a ...any) { problems = append(problems, f) })
	if len(problems) != 0 || r.Site.PendingMigrations == nil || *r.Site.PendingMigrations != 1 ||
		r.Site.CronLastRun == nil || !r.Site.CronLastRun.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("%+v %v", r.Site, problems)
	}

	if err := os.Remove(filepath.Join(dir, consoleBin)); err != nil {
		t.Fatal(err)
	}
	r, problems = report.Report{}, nil
	collectContaoConsole(context.Background(), Config{Path: dir, PHP: php}, &r, now, func(f string, a ...any) { problems = append(problems, f) })
	if len(problems) != 1 || !strings.Contains(problems[0], "nicht gefunden") || r.Site.PendingMigrations != nil {
		t.Errorf("ohne Konsole: %v %+v", problems, r.Site)
	}
}

func TestCollectMaintenance(t *testing.T) {
	dir := t.TempDir()
	var problems int
	problem := func(string, ...any) { problems++ }
	var r report.Report
	collectMaintenance(Config{Path: dir}, &r, problem)
	if r.Site.MaintenanceMode == nil || *r.Site.MaintenanceMode {
		t.Fatalf("ohne Datei: %v", r.Site.MaintenanceMode)
	}
	if err := os.MkdirAll(filepath.Join(dir, "var"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, maintenanceFile), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	collectMaintenance(Config{Path: dir}, &r, problem)
	if r.Site.MaintenanceMode == nil || !*r.Site.MaintenanceMode || r.Site.MaintenanceSince == nil || problems != 0 {
		t.Fatalf("mit Datei: %v, Probleme %d", r.Site.MaintenanceMode, problems)
	}
}
