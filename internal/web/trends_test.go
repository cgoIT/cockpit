package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sitewatch/internal/report"
	"sitewatch/internal/status"
	"sitewatch/internal/store"
)

func ip(n int) *int { return &n }

func TestBuildChart(t *testing.T) {
	to := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -10)
	at := func(d int) time.Time { return from.AddDate(0, 0, d).Add(time.Hour) }
	samples := []store.Sample{
		{At: at(1), DiskFreePct: ip(60)},
		{At: at(1).Add(time.Minute), DiskFreePct: ip(40)}, // gleicher Slot: Minimum gewinnt
		{At: at(5)}, // kein Wert: Lücke
		{At: at(8), DiskFreePct: ip(30)},
		{At: from.Add(-time.Hour), DiskFreePct: ip(1)}, // vor dem Zeitraum
	}
	c := buildChart(metrics[0], samples, from, to)
	if c.Empty || c.Last != "30 %" || c.Min != "30 %" || c.Max != "60 %" {
		t.Fatalf("%+v", c)
	}
	if strings.Count(c.Path, "M") != 2 { // zwei getrennte Stücke, nichts wird über die Lücke gezogen
		t.Errorf("Pfad: %s", c.Path)
	}
	two := []store.Sample{{At: at(1), DiskFreePct: ip(50)}, {At: at(6), DiskFreePct: ip(45)}}
	if c := buildChart(metrics[0], two, from, to); strings.Count(c.Path, "M") != 1 || strings.Contains(c.Path, "h0.1") {
		t.Errorf("Zeit zwischen zwei Meldungen muss überbrückt werden: %s", c.Path)
	}
	if e := buildChart(metrics[0], []store.Sample{{At: at(1)}}, from, to); !e.Empty {
		t.Error("ohne Werte muss das Diagramm leer sein")
	}
}

func TestBandAndVersionChanges(t *testing.T) {
	to := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -10)
	at := func(d int) time.Time { return from.AddDate(0, 0, d) }
	samples := []store.Sample{
		{At: at(1), Level: 0, PHP: "8.3.1", Contao: "5.6.1"},
		{At: at(2), Level: 0, PHP: "8.3.2"},
		{At: at(3), Level: 2, PHP: "8.4.0", Contao: "5.7.1"},
		{At: at(4), Level: 0, PHP: ""}, // unbekannt zählt nicht als Wechsel
		{At: at(5), Level: 0, PHP: "8.4.1"},
	}
	band := buildBand(samples, from, to, 0, time.Time{})
	if len(band) != 3 || band[0].Level != 0 || band[1].Level != 2 || band[2].Level != 0 {
		t.Errorf("Band (gleiche Status verschmelzen): %+v", band)
	}
	ch := versionChanges(samples)
	// neueste zuerst: PHP 8.3.2 -> 8.4.0 und Contao 5.6.1 -> 5.7.1 am selben Punkt, davor PHP 8.3.1 -> 8.3.2
	if len(ch) != 4 || ch[0].To != "8.4.1" || ch[len(ch)-1].From != "8.3.1" {
		t.Errorf("Versionswechsel: %+v", ch)
	}
}

func TestBandGap(t *testing.T) {
	to := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	from := to.Add(-4 * time.Hour)
	samples := []store.Sample{
		{At: from.Add(time.Hour), Level: 0},
		{At: from.Add(time.Hour + 5*time.Minute), Level: 0},
		{At: from.Add(3 * time.Hour), Level: 1}, // 2 Std. Pause davor, danach bis jetzt still
	}
	band := buildBand(samples, from, to, 15*time.Minute, from)
	var lv []int
	for _, b := range band {
		lv = append(lv, b.Level)
	}
	if want := []int{0, levelGap, 1, levelGap}; len(lv) != 4 || lv[0] != want[0] || lv[1] != want[1] || lv[2] != want[2] || lv[3] != want[3] {
		t.Errorf("Stufen = %v, erwartet %v", lv, want)
	}
	if old := buildBand(samples, from, to, 15*time.Minute, to); len(old) != 2 { // vor gapFrom keine Lücken (verdichtet): 0 und 1
		t.Errorf("verdichtete Punkte dürfen keine Lücke zeigen: %+v", old)
	}
}

func TestHoursTextUsesComma(t *testing.T) {
	if got := hoursText(1.5); got != "1,5 Std." {
		t.Errorf("%q", got)
	}
}

func TestNiceCeil(t *testing.T) {
	for in, want := range map[float64]float64{0.9: 1, 1: 1, 1.2: 2, 3: 5, 7: 10, 11: 20, 33: 50, 0.13: 0.2, 100: 100, 101: 200} {
		if got := niceCeil(in); got != want {
			t.Errorf("niceCeil(%v) = %v, erwartet %v", in, got, want)
		}
	}
}

func TestBuildTrendHidesPointlessCharts(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	hist := []store.Sample{{At: now.Add(-time.Hour), DiskFreePct: ip(40)}, {At: now.Add(-2 * time.Hour), DiskFreePct: ip(41)}}
	titles := func(tr trend) []string {
		var out []string
		for _, c := range tr.Charts {
			out = append(out, c.Title)
		}
		return out
	}
	if got := titles(buildTrend(hist, 30, now, false, status.DefaultConfig())); len(got) != 1 || got[0] != "Speicherplatz frei" { // kein Cron, kein Backup, nicht Contao
		t.Errorf("Host/PHP: %v", got)
	}
	if got := titles(buildTrend(hist, 30, now, true, status.DefaultConfig())); len(got) != 3 { // dazu Updates und Fehler
		t.Errorf("Contao: %v", got)
	}
	tr := buildTrend(hist, 30, now, false, status.DefaultConfig()) // Zeitraum beginnt immer vor 30 Tagen, auch bei kurzer Historie
	if tr.XLabels[0] != now.AddDate(0, 0, -30).Local().Format("02.01.") {
		t.Errorf("x-Achse: %v", tr.XLabels)
	}
	if got := buildTrend(nil, 30, now, true, status.DefaultConfig()); len(got.Charts) != 0 { // ohne Daten kein Absturz
		t.Errorf("leer: %v", titles(got))
	}
}

func TestRangeDays(t *testing.T) {
	for q, want := range map[string]int{"": 30, "1": 1, "7": 7, "365": 365, "12": 30, "abc": 30} {
		r := httptest.NewRequest(http.MethodGet, "/sites/x?range="+q, nil)
		if got := rangeDays(r); got != want {
			t.Errorf("range=%q: %d, erwartet %d", q, got, want)
		}
	}
}

func TestDetailShowsTrend(t *testing.T) {
	srv, tok, now := setup(t)
	for i := 0; i < 3; i++ {
		d := 25 - 10*i
		rep := report.Report{Schema: 1, CollectedAt: now.Add(-time.Duration(d) * time.Hour), Host: report.Host{DiskFreePct: ip(50 - i)},
			Site: report.Site{ID: "kunde-c", Name: "Kunde C", Kind: "contao", PHP: []string{"8.3.1", "8.3.1", "8.4.0"}[i]}}
		if w := post(srv, tok, rep); w.Code != http.StatusNoContent {
			t.Fatalf("Ingest: %d %s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sites/kunde-c?range=7", nil))
	body := w.Body.String()
	for _, want := range []string{"letzte 7 Tage", `<path d="M`, "Speicherplatz frei", "PHP", "8.3.1 → 8.4.0", `class="on"`} {
		if !strings.Contains(body, want) {
			t.Errorf("fehlt %q", want)
		}
	}
}
