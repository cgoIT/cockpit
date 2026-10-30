package web

import (
	"testing"
	"time"

	"cockpit/internal/report"
	"cockpit/internal/status"
	"cockpit/internal/store"
)

func TestBuildMatrices(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	disk, mig := 8, 2
	cronOld := now.Add(-5 * time.Hour)
	entries := []store.Entry{
		{Report: report.Report{
			Host: report.Host{Nginx: "1.26", DiskFreePct: &disk},
			Site: report.Site{ID: "b", Name: "B", PHP: "8.1.9", Contao: "5.3.4", LastBackup: &old, CronLastRun: &cronOld, PendingMigrations: &mig},
		}},
		{Report: report.Report{Site: report.Site{ID: "a", Name: "A", Kind: "host"}}},
	}
	v, p := buildMatrices(entries, []string{"a", "b", "c"}, now, status.DefaultConfig(), nil)

	if len(v.Rows) != 3 || v.Rows[0].Name != "A" || v.Rows[2].Name != "c" { // sortiert, Site ohne Meldung dabei
		t.Fatalf("Zeilen: %+v", v.Rows)
	}
	b := v.Rows[1].Cells // Contao, PHP, MariaDB, Nginx
	if b[0].Text != "5.3.4" || b[1].Level != "warn" || b[2].Text != "–" || b[3].Text != "1.26" {
		t.Fatalf("Versionen B: %+v", b)
	}
	if c := v.Rows[0].Cells[0]; c.Text != "–" || c.Level == "warn" || c.Level == "crit" {
		t.Fatalf("fehlender Wert: %+v", c)
	}
	if c, m := p.Rows[1].Cells[3], p.Rows[1].Cells[4]; c.Level != "crit" || m.Text != "2" || m.Level != "crit" {
		t.Fatalf("Cron/Migrationen B: %+v %+v", c, m)
	}
	pb := p.Rows[1].Cells // Platz, Backup, TLS
	if pb[0].Text != "8 %" || pb[0].Level != "warn" || pb[1].Level != "warn" || pb[2].Text != "–" {
		t.Fatalf("Parameter B: %+v", pb)
	}
}
