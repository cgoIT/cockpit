package web

import (
	"strings"
	"testing"
	"time"

	"cockpit/internal/store"
)

func TestTrendArrow(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	week := now.Add(-6 * 24 * time.Hour)
	cases := []struct {
		name        string
		cur, base   int
		baseAt      time.Time
		goodUp      bool
		min         int
		text, class string // text "" = kein Pfeil
	}{
		{"Speicher sinkt", 25, 29, week, true, 2, "↓ 4", "bad"},
		{"Speicher steigt", 40, 30, week, true, 2, "↑ 10", "good"},
		{"Rauschen", 29, 30, week, true, 2, "", ""},
		{"zu junge Historie", 10, 30, now.Add(-2 * time.Hour), true, 2, "", ""},
		{"Updates steigen", 5, 2, week, false, 1, "↑ 3", "bad"},
		{"Updates sinken", 0, 2, week, false, 1, "↓ 2", "good"},
		{"unverändert", 2, 2, week, false, 1, "", ""},
	}
	for _, c := range cases {
		a := trendArrow(c.cur, c.base, c.baseAt, now, c.goodUp, c.min, count)
		if c.text == "" {
			if a != nil {
				t.Errorf("%s: erwartet kein Pfeil, got %+v", c.name, a)
			}
			continue
		}
		if a == nil || a.Text != c.text || a.Class != c.class {
			t.Errorf("%s: got %+v", c.name, a)
		}
	}
}

func TestDiskForecast(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	base := func(pct int, days int) store.Sample {
		return store.Sample{At: now.AddDate(0, 0, -days), DiskFreePct: &pct}
	}
	cur := 30
	// 10 Punkte in 5 Tagen = 2 pp/Tag: 15 Tage
	if a := diskArrow(&cur, base(40, 5), true, now); a == nil || !strings.Contains(a.Text, "voll in ~15 Tg.") {
		t.Errorf("%+v", a)
	}
	// 2 pp in 7 Tagen: weit über 90 Tage, keine Prognose, aber der Pfeil bleibt
	cur = 50
	if a := diskArrow(&cur, base(52, 7), true, now); a == nil || strings.Contains(a.Text, "voll") {
		t.Errorf("%+v", a)
	}
	// steigender freier Platz: keine Prognose
	cur = 60
	if a := diskArrow(&cur, base(50, 3), true, now); a == nil || strings.Contains(a.Text, "voll") {
		t.Errorf("%+v", a)
	}
}
