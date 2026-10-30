package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sitewatch/internal/report"
)

func open(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "x", "c.db"), DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func entry(id string, at time.Time, updates int, disk *int) Entry {
	out := make([]report.Package, updates)
	return Entry{ReceivedAt: at, Report: report.Report{CollectedAt: at, Host: report.Host{DiskFreePct: disk},
		Site: report.Site{ID: id, Name: id, Outdated: out}}}
}

func TestSaveLatestGetHistory(t *testing.T) {
	s := open(t)
	t0 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	for i, u := range []int{1, 2, 3} {
		if err := s.Save(entry("a", t0.Add(time.Duration(i)*time.Hour), u, nil), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(entry("b", t0.Add(24*time.Hour), 0, nil), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(entry("../x", t0, 0, nil), 0); err == nil {
		t.Error("ungültige Site-ID angenommen")
	}
	latest, err := s.Latest()
	if err != nil || len(latest) != 2 || latest[0].Report.Site.ID != "a" || latest[0].Report.Site.OpenUpdates() != 3 {
		t.Fatalf("Latest: %+v %v", latest, err)
	}
	if e, ok, _ := s.Get("b"); !ok || !e.ReceivedAt.Equal(t0.Add(24*time.Hour)) {
		t.Errorf("Get: %+v %v", e, ok)
	}
	if _, ok, _ := s.Get("fehlt"); ok {
		t.Error("Get: unbekannte Site")
	}
	hist, _ := s.History("a", 2)
	if len(hist) != 2 || hist[0].OpenUpdates != 3 || hist[1].OpenUpdates != 2 { // neueste zuerst
		t.Errorf("History: %+v", hist)
	}
}

func TestOlderReportDoesNotReplaceDailyState(t *testing.T) {
	s := open(t)
	t0 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	newer := entry("a", t0.Add(time.Hour), 5, nil)
	older := entry("a", t0, 1, nil)
	older.ReceivedAt = t0.Add(2 * time.Hour) // später eingetroffen (nachgesendet)
	s.Save(newer, 0)
	s.Save(older, 0)
	if e, _, _ := s.Get("a"); e.Report.Site.OpenUpdates() != 5 {
		t.Errorf("älterer Report hat den Tagesstand überschrieben: %d", e.Report.Site.OpenUpdates())
	}
}

func TestMaintain(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	d := func(n int) float64 { return float64(n) }
	at := func(days float64, hours int) time.Time { // days Tage vor now, auf 00:00 UTC, plus hours
		return now.Add(-time.Duration(days*24) * time.Hour).Truncate(24 * time.Hour).Add(time.Duration(hours) * time.Hour)
	}
	disk := func(n int) *int { return &n }
	// 45 Tage alt: zwei Punkte im selben 6-Stunden-Block (Status 0 und 2, Speicher 40 und 30) und einer im nächsten
	s.Save(entry("a", at(d(45), 1), 1, disk(40)), 0)
	s.Save(entry("a", at(d(45), 2), 2, disk(30)), 2)
	s.Save(entry("a", at(d(45), 7), 3, disk(50)), 0)
	// 100 Tage alt: drei Punkte an einem Tag -> einer
	s.Save(entry("a", at(d(100), 1), 1, nil), 0)
	s.Save(entry("a", at(d(100), 9), 2, nil), 0)
	s.Save(entry("a", at(d(100), 20), 3, nil), 1)
	// 400 Tage alt: weg; 10 Tage alt: unverändert
	s.Save(entry("a", at(d(400), 1), 1, nil), 0)
	s.Save(entry("a", at(d(10), 1), 1, nil), 0)
	s.Save(entry("a", at(d(10), 2), 1, nil), 0)

	for i := 0; i < 2; i++ { // zweiter Lauf: nichts ändert sich mehr
		if err := s.Maintain(now); err != nil {
			t.Fatal(err)
		}
		hist, _ := s.History("a", 0)
		if len(hist) != 5 {
			t.Fatalf("Lauf %d: %d Punkte, erwartet 5: %+v", i+1, len(hist), hist)
		}
	}
	hist, _ := s.History("a", 0) // neueste zuerst: 10 Tage (2), 45 Tage (Block 7 Uhr, Block 1-2 Uhr), 100 Tage
	blk := hist[3]               // der Block mit den zwei Punkten
	if blk.Level != 2 || blk.DiskFreePct == nil || *blk.DiskFreePct != 30 || blk.OpenUpdates != 2 {
		t.Errorf("Block: schlimmster Status, kleinster Speicher, letzter Wert: %+v", blk)
	}
	if day := hist[4]; day.Level != 1 || day.OpenUpdates != 3 {
		t.Errorf("Tagesstand: %+v", day)
	}
}

func TestMaintainReportsKeepNewestPerSite(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s.Save(entry("alt", now.AddDate(0, 0, -200), 1, nil), 0) // einziger Report, älter als ReportDays
	s.Save(entry("neu", now.AddDate(0, 0, -200), 1, nil), 0)
	s.Save(entry("neu", now.AddDate(0, 0, -1), 2, nil), 0)
	if err := s.Maintain(now); err != nil {
		t.Fatal(err)
	}
	latest, _ := s.Latest()
	if len(latest) != 2 { // "alt" bleibt, sonst verschwände die stille Site
		t.Fatalf("Latest: %d", len(latest))
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM daily_reports WHERE site_id='neu'`).Scan(&n)
	if n != 1 {
		t.Errorf("alter Tagesstand von neu nicht gelöscht: %d", n)
	}
}

func TestImportFilesOnce(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	var lines []byte
	for i := 0; i < 3; i++ {
		b, _ := json.Marshal(entry("a", t0.Add(time.Duration(i)*time.Hour), i, nil))
		lines = append(lines, append(b, '\n')...)
	}
	lines = append(lines, "kaputt\n"...)
	os.WriteFile(filepath.Join(dir, "a.history.jsonl"), lines, 0o600)
	latest, _ := json.Marshal(entry("b", t0, 0, nil))
	os.WriteFile(filepath.Join(dir, "b.latest.json"), latest, 0o600) // Site nur mit latest

	s := open(t)
	n, err := s.ImportFiles(dir, func(Entry) int { return 1 })
	if err != nil || n != 4 {
		t.Fatalf("Import: %d %v", n, err)
	}
	if l, _ := s.Latest(); len(l) != 2 {
		t.Errorf("Latest nach Import: %d", len(l))
	}
	if h, _ := s.History("a", 0); len(h) != 3 || h[0].Level != 1 {
		t.Errorf("History nach Import: %+v", h)
	}
	if n, _ := s.ImportFiles(dir, func(Entry) int { return 0 }); n != 0 {
		t.Errorf("zweiter Import hat %d Reports gelesen", n)
	}
}

func TestAlertState(t *testing.T) {
	s := open(t)
	s.SetAlerted("a", true)
	s.SetAlerted("b", true)
	s.SetAlerted("a", false)
	got, err := s.AlertState()
	if err != nil || len(got) != 1 || !got["b"] {
		t.Errorf("%v %v", got, err)
	}
}

func TestBaselines(t *testing.T) {
	s := open(t)
	t0 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	for i, u := range []int{5, 6, 7} { // a: drei Punkte, der älteste im Fenster zählt
		if err := s.Save(entry("a", t0.Add(time.Duration(i)*24*time.Hour), u, nil), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(entry("b", t0, 1, nil), 0); err != nil { // b: vor dem Fenster
		t.Fatal(err)
	}
	b, err := s.Baselines(t0.Add(12 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := b["a"]; got.OpenUpdates != 6 || !got.At.Equal(t0.Add(24*time.Hour)) {
		t.Errorf("a = %+v", got)
	}
	if _, ok := b["b"]; ok {
		t.Error("b hat keinen Punkt im Fenster")
	}
}

func TestDays(t *testing.T) {
	s := open(t)
	t0 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	for i := range 3 {
		if err := s.Save(entry("a", t0.Add(time.Duration(i)*24*time.Hour), i, nil), 0); err != nil {
			t.Fatal(err)
		}
	}
	days, err := s.Days("a")
	if err != nil || len(days) != 3 || days[0] != "2026-10-29" || days[2] != "2026-10-27" {
		t.Fatalf("Days = %v, %v", days, err)
	}
	e, ok, err := s.GetDay("a", "2026-10-28")
	if err != nil || !ok || len(e.Report.Site.Outdated) != 1 {
		t.Errorf("GetDay: %+v %v %v", e, ok, err)
	}
	if _, ok, _ := s.GetDay("a", "2020-01-01"); ok {
		t.Error("unbekannter Tag")
	}
}
