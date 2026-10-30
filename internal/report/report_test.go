package report

import (
	"testing"
	"time"
)

func valid(now time.Time) Report {
	return Report{Schema: 1, Site: Site{ID: "kunde-c", Kind: "contao"}, CollectedAt: now}
}

func TestValidate(t *testing.T) {
	now := time.Date(2026, 10, 27, 19, 0, 0, 0, time.UTC)
	r := valid(now)
	if err := r.Validate(now); err != nil {
		t.Fatalf("gültiger Report abgelehnt: %v", err)
	}
	r.Site.Kind = "host"
	if err := r.Validate(now); err != nil {
		t.Fatalf("kind host abgelehnt: %v", err)
	}
	cases := map[string]func(*Report){
		"schema":         func(r *Report) { r.Schema = 2 },
		"id":             func(r *Report) { r.Site.ID = "../etc" },
		"kind":           func(r *Report) { r.Site.Kind = "wordpress" },
		"zukunft":        func(r *Report) { r.CollectedAt = now.Add(time.Hour) },
		"zu alt":         func(r *Report) { r.CollectedAt = now.Add(-8 * 24 * time.Hour) },
		"backup negativ": func(r *Report) { n := -1; r.Site.BackupCount = &n },
		"backup zukunft": func(r *Report) { t := now.Add(time.Hour); r.Site.LastBackup = &t },
		"zeit leer":      func(r *Report) { r.CollectedAt = time.Time{} },
	}
	for name, mutate := range cases {
		r := valid(now)
		mutate(&r)
		if err := r.Validate(now); err == nil {
			t.Errorf("%s: Fehler erwartet", name)
		}
	}
}
