package status

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"sitewatch/internal/report"
)

func TestDiskLevel(t *testing.T) {
	cfg := DefaultConfig() // 15 / 5
	for pct, want := range map[int]Level{50: OK, 15: OK, 14: Warn, 5: Warn, 4: Crit, 0: Crit} {
		if got := DiskLevel(pct, cfg); got != want {
			t.Errorf("%d %%: %v, erwartet %v", pct, got, want)
		}
	}
}

func TestCronAndMigrationsEvaluate(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	old, fresh, n := now.Add(-3*time.Hour), now.Add(-time.Minute), 2
	r := &report.Report{Site: report.Site{CronLastRun: &old, PendingMigrations: &n}}
	res := Evaluate(r, now, now, cfg)
	if res.Level != Crit || len(res.Reasons) != 2 { // offene Migrationen sind immer kritisch
		t.Fatalf("%+v", res)
	}
	veryOld := now.Add(-5 * time.Hour)
	r = &report.Report{Site: report.Site{CronLastRun: &veryOld}}
	if res := Evaluate(r, now, now, cfg); res.Level != Crit {
		t.Fatalf("5 Std.: %+v", res)
	}
	zero := 0
	r = &report.Report{Site: report.Site{CronLastRun: &fresh, PendingMigrations: &zero}}
	if res := Evaluate(r, now, now, cfg); res.Level != OK {
		t.Fatalf("%+v", res)
	}
}

func TestMaintenanceEvaluate(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig() // 2 Std. Warnung, 8 Std. kritisch
	on := true
	for since, want := range map[time.Duration]Level{time.Hour: OK, 3 * time.Hour: Warn, 9 * time.Hour: Crit} {
		s := now.Add(-since)
		r := &report.Report{Site: report.Site{MaintenanceMode: &on, MaintenanceSince: &s}}
		if got := Evaluate(r, now, now, cfg).Level; got != want {
			t.Errorf("seit %v: %v, erwartet %v", since, got, want)
		}
	}
	off, s := false, now.Add(-10*time.Hour) // ausgeschaltet: kein Urteil, auch bei altem Zeitpunkt
	if got := Evaluate(&report.Report{Site: report.Site{MaintenanceMode: &off, MaintenanceSince: &s}}, now, now, cfg).Level; got != OK {
		t.Errorf("aus: %v", got)
	}
}

func TestLoadThresholds(t *testing.T) {
	write := func(s string) string {
		p := filepath.Join(t.TempDir(), "t.json")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	def := DefaultConfig()
	if c, err := LoadThresholds("", def); err != nil || c != def {
		t.Fatalf("leerer Pfad: %+v %v", c, err)
	}
	c, err := LoadThresholds(write(`{"tls_warn_days":30,"disk_warn_pct":20}`), def)
	if err != nil || c.TLSWarn != 30*24*time.Hour || c.DiskWarn != 20 || c.DiskCrit != def.DiskCrit || c.BackupMax != def.BackupMax {
		t.Fatalf("%+v %v", c, err)
	}
	for name, content := range map[string]string{
		"unbekannt": `{"tls_warn":30}`,
		"null":      `{"tls_warn_days":0}`,
		"crit>warn": `{"disk_crit_pct":30}`,
		"cron":      `{"cron_warn_minutes":300}`,
		"wartung":   `{"maintenance_warn_minutes":600}`,
		"history":   `{"history_full_days":100}`,
		">100":      `{"disk_warn_pct":101}`,
		"kein JSON": `tls`,
	} {
		if _, err := LoadThresholds(write(content), def); err == nil {
			t.Errorf("%s: kein Fehler", name)
		}
	}
}

type fakeEOL map[string]time.Time

func (f fakeEOL) EOL(product, version string) (time.Time, bool) {
	t, ok := f[product+" "+minor(version)]
	return t, ok
}

func TestEOLLevel(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	if l, _ := EOLLevel("php", "8.1.9", now, cfg); l != Warn { // ohne Quelle: eingebaute Tabelle
		t.Errorf("Tabelle: %v", l)
	}
	if l, _ := EOLLevel("contao", "5.6.1", now, cfg); l != OK { // ohne Daten kein Urteil
		t.Errorf("ohne Quelle: %v", l)
	}
	cfg.EOL = fakeEOL{"contao 5.6": now.Add(-time.Hour), "contao 5.7": now.Add(400 * 24 * time.Hour), "php 8.1": now.Add(400 * 24 * time.Hour)}
	if l, _ := EOLLevel("contao", "5.6.1", now, cfg); l != Warn {
		t.Errorf("abgelaufen: %v", l)
	}
	if l, _ := EOLLevel("contao", "5.7.15", now, cfg); l != OK {
		t.Errorf("weit entfernt: %v", l)
	}
	if l, _ := EOLLevel("php", "8.1.9", now, cfg); l != OK { // die Quelle hat Vorrang vor der Tabelle
		t.Errorf("Quelle vor Tabelle: %v", l)
	}
}
