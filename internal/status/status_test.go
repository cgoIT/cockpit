package status

import (
	"testing"
	"time"

	"sitewatch/internal/report"
)

var now = time.Date(2026, 10, 27, 19, 0, 0, 0, time.UTC)

func base() *report.Report {
	return &report.Report{Site: report.Site{ID: "x", Kind: "contao", PHP: "8.3.12"}}
}

func TestEvaluate(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name string
		mod  func(*report.Report)
		age  time.Duration
		want Level
	}{
		{"ok", func(r *report.Report) {}, time.Minute, OK},
		{"veraltet", func(r *report.Report) {}, time.Hour, Crit},
		{"advisory", func(r *report.Report) { r.Site.Advisories = []report.Advisory{{Package: "a"}} }, time.Minute, Crit},
		{"update", func(r *report.Report) { r.Site.Outdated = []report.Package{{Name: "a"}} }, time.Minute, Warn},
		{"php eol", func(r *report.Report) { r.Site.PHP = "8.1.30" }, time.Minute, Warn},
		{"php bald eol", func(r *report.Report) { r.Site.PHP = "8.2.24" }, time.Minute, Warn},
		{"viele fehler", func(r *report.Report) { r.Errors = []report.LogError{{Count: 12}} }, time.Minute, Warn},
		{"wenige fehler", func(r *report.Report) { r.Errors = []report.LogError{{Count: 2}} }, time.Minute, OK},
		{"tls abgelaufen", func(r *report.Report) { r.Site.TLSExpires = ptr(now.Add(-time.Hour)) }, time.Minute, Crit},
		{"tls bald", func(r *report.Report) { r.Site.TLSExpires = ptr(now.Add(5 * 24 * time.Hour)) }, time.Minute, Warn},
		{"tls ok", func(r *report.Report) { r.Site.TLSExpires = ptr(now.Add(60 * 24 * time.Hour)) }, time.Minute, OK},
		{"backup alt", func(r *report.Report) { r.Site.LastBackup = ptr(now.Add(-3 * 24 * time.Hour)) }, time.Minute, Warn},
		{"backup frisch", func(r *report.Report) { r.Site.LastBackup = ptr(now.Add(-2 * time.Hour)) }, time.Minute, OK},
		{"tls genau an der Schwelle", func(r *report.Report) { r.Site.TLSExpires = ptr(now.Add(cfg.TLSWarn)) }, time.Minute, OK},
		{"backup genau an der Schwelle", func(r *report.Report) { r.Site.LastBackup = ptr(now.Add(-cfg.BackupMax)) }, time.Minute, OK},
		{"nur ausgeschlossene Updates", func(r *report.Report) { r.Site.Outdated = []report.Package{{Name: "a", Ignored: true}} }, time.Minute, OK},
		{"gemischte Updates", func(r *report.Report) {
			r.Site.Outdated = []report.Package{{Name: "a", Ignored: true}, {Name: "b"}}
		}, time.Minute, Warn},
		{"probleme", func(r *report.Report) { r.Site.Problems = []string{"x"} }, time.Minute, Warn},
	}
	for _, c := range cases {
		r := base()
		c.mod(r)
		got := Evaluate(r, now.Add(-c.age), now, cfg)
		if got.Level != c.want {
			t.Errorf("%s: Level %v, erwartet %v (%v)", c.name, got.Level, c.want, got.Reasons)
		}
	}
}

func ptr(t time.Time) *time.Time { return &t }
