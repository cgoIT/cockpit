package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"cockpit/internal/status"
	"cockpit/internal/store"
)

// Die Kurven sind serverseitig gerenderte SVG-Pfade (kein JavaScript, passt zur CSP).
const (
	chartW, chartH = 600.0, 80.0
	chartSlots     = 400 // mehr Punkte als Pixel bringen nichts; je Slot zählt der schlimmste Wert
	bandH          = 14.0
)

type rangeChoice struct {
	Days  int
	Label string
}

// Der kürzeste Zeitraum (24 Stunden) zeigt auch bei junger Historie schon etwas Sinnvolles.
var rangeChoices = []rangeChoice{{1, "24 Std."}, {7, "7 Tage"}, {30, "30 Tage"}, {90, "90 Tage"}, {365, "365 Tage"}}

// rangeDays liest ?range=; nur die angebotenen Zeiträume sind erlaubt, sonst 30 Tage.
func rangeDays(r *http.Request) int {
	for _, c := range rangeChoices {
		if r.URL.Query().Get("range") == fmt.Sprint(c.Days) {
			return c.Days
		}
	}
	return 30
}

type metric struct {
	title    string
	get      func(store.Sample) (float64, bool)
	format   func(float64) string
	fixedMax float64 // 0 = aus den Daten
	lowIsBad bool    // im Slot gilt das Minimum (freier Speicher) statt des Maximums
	contao   bool    // nur für Contao-Sites sinnvoll (sonst immer 0)
}

func intVal(p *int) (float64, bool) {
	if p == nil {
		return 0, false
	}
	return float64(*p), true
}

func hours(p *int) (float64, bool) {
	if p == nil {
		return 0, false
	}
	return float64(*p) / 3600, true
}

// hoursText formatiert Stunden mit deutschem Dezimalkomma ("1,5 Std."). Der SVG-Pfad bleibt bei Punkt, das ist keine Anzeige.
func hoursText(v float64) string { return strings.Replace(fmt.Sprintf("%.1f Std.", v), ".", ",", 1) }

var metrics = []metric{
	{"Speicherplatz frei", func(s store.Sample) (float64, bool) { return intVal(s.DiskFreePct) }, func(v float64) string { return fmt.Sprintf("%.0f %%", v) }, 100, true, false},
	{"Offene Updates", func(s store.Sample) (float64, bool) { return float64(s.OpenUpdates), true }, func(v float64) string { return fmt.Sprintf("%.0f", v) }, 0, false, true},
	{"Fehler (24 Std.)", func(s store.Sample) (float64, bool) { return float64(s.Errors), true }, func(v float64) string { return fmt.Sprintf("%.0f", v) }, 0, false, true},
	{"Alter des letzten Cron-Laufs", func(s store.Sample) (float64, bool) { return hours(s.CronAgeSec) }, hoursText, 0, false, false},
	{"Alter des letzten Backups", func(s store.Sample) (float64, bool) { return hours(s.BackupAgeSec) }, hoursText, 0, false, false},
}

type chart struct {
	Title, Path    string
	Last, Min, Max string
	YTop, YMid     string // Beschriftung der y-Achse; unten steht 0
	Empty          bool
}

// niceCeil rundet auf 1, 2, 5 oder 10 mal einer Zehnerpotenz auf, damit die Achse runde Werte zeigt.
func niceCeil(x float64) float64 {
	p := 1.0
	for p*10 <= x {
		p *= 10
	}
	for p > x { // x < 1
		p /= 10
	}
	for _, f := range []float64{1, 2, 5, 10} {
		if f*p >= x {
			return f * p
		}
	}
	return 10 * p
}

// buildChart zeichnet die Werte im Zeitraum [from, to]. samples ist nach Zeit aufsteigend sortiert.
func buildChart(m metric, samples []store.Sample, from, to time.Time) chart {
	c := chart{Title: m.title, Empty: true}
	if to.Sub(from) < time.Hour { // Schutz gegen Division durch 0 bei nur einem Punkt
		from = to.Add(-time.Hour)
	}
	slot := to.Sub(from) / chartSlots
	vals, has := make([]float64, chartSlots), make([]bool, chartSlots)
	brk := make([]bool, chartSlots) // an dieser Stelle fehlte der Wert (nil): die Linie wird unterbrochen
	var last, lo, hi float64
	for _, s := range samples {
		if s.At.Before(from) || s.At.After(to) {
			continue
		}
		i := min(int(s.At.Sub(from)/slot), chartSlots-1)
		v, ok := m.get(s)
		if !ok {
			brk[i] = true
			continue
		}
		switch {
		case !has[i]:
			vals[i], has[i] = v, true
		case m.lowIsBad:
			vals[i] = min(vals[i], v)
		default:
			vals[i] = max(vals[i], v)
		}
		if c.Empty {
			lo, hi, c.Empty = v, v, false
		}
		last, lo, hi = v, min(lo, v), max(hi, v)
	}
	if c.Empty {
		return c
	}
	c.Last, c.Min, c.Max = m.format(last), m.format(lo), m.format(hi)
	ymax := m.fixedMax
	if ymax == 0 {
		ymax = niceCeil(max(hi, 1))
	}
	c.YTop, c.YMid = m.format(ymax), m.format(ymax/2)
	// Leere Slots (Zeit zwischen zwei Meldungen) werden überbrückt; nur fehlende Werte trennen die Linie.
	// shortcut: längere Ausfälle des Agenten werden ebenfalls überbrückt; bei Bedarf Lücken über einem Vielfachen des Meldeintervalls trennen.
	type pt struct {
		x, y  float64
		start bool
	}
	var pts []pt
	pending := true
	for i := range has {
		pending = pending || brk[i]
		if !has[i] {
			continue
		}
		pts = append(pts, pt{(float64(i) + 0.5) / chartSlots * chartW, chartH - 2 - vals[i]/ymax*(chartH-4), pending})
		pending = false
	}
	var p strings.Builder
	for k, q := range pts {
		cmd := "L"
		if q.start {
			cmd = "M"
		}
		fmt.Fprintf(&p, "%s%.1f,%.1f", cmd, q.x, q.y)
		if q.start && (k == len(pts)-1 || pts[k+1].start) {
			p.WriteString("h0.1") // einzelner Punkt: sonst unsichtbar
		}
	}
	c.Path = p.String()
	return c
}

type bandSeg struct {
	X, W  float64
	Level int
}

// levelGap kennzeichnet im Statusband Zeiten ohne Meldung (grau).
const levelGap = 3

// buildBand liefert das Statusband: aufeinanderfolgende Punkte mit gleichem Status werden zu einem Streifen.
// Liegt zwischen zwei Punkten mehr als gap, gilt der Status noch bis At+gap, danach ist der Streifen grau.
// Nur Punkte ab gapFrom werden so beurteilt: ältere sind zu 6-Stunden-Blöcken bzw. Tagen verdichtet, deren Abstand ist normal.
func buildBand(samples []store.Sample, from, to time.Time, gap time.Duration, gapFrom time.Time) []bandSeg {
	pos := func(t time.Time) float64 {
		return min(max(float64(t.Sub(from))/float64(to.Sub(from)), 0), 1) * chartW
	}
	var segs []bandSeg
	add := func(t0, t1 time.Time, level int) {
		x0, x1 := pos(t0), pos(t1)
		if n := len(segs); n > 0 && segs[n-1].Level == level && segs[n-1].X+segs[n-1].W >= x0 {
			segs[n-1].W = x1 - segs[n-1].X
			return
		}
		segs = append(segs, bandSeg{X: x0, W: x1 - x0, Level: level})
	}
	for i, s := range samples {
		if s.At.Before(from) || s.At.After(to) {
			continue
		}
		end := to
		if i+1 < len(samples) {
			end = samples[i+1].At
		}
		if gap > 0 && !s.At.Before(gapFrom) && end.Sub(s.At) > gap {
			add(s.At, s.At.Add(gap), s.Level)
			add(s.At.Add(gap), end, levelGap)
			continue
		}
		add(s.At, end, s.Level)
	}
	return segs
}

type versionChange struct {
	At       time.Time
	Product  string
	From, To string
}

// versionChanges erkennt Wechsel der Versionen (nur zwischen zwei bekannten Werten); neueste zuerst, höchstens 20.
// Bei verdichteten Punkten ist die Zeit auf Block bzw. Tag genau.
func versionChanges(samples []store.Sample) []versionChange {
	type tracker struct {
		name string
		get  func(store.Sample) string
		prev string
	}
	ts := []*tracker{
		{name: "Contao", get: func(s store.Sample) string { return s.Contao }},
		{name: "PHP", get: func(s store.Sample) string { return s.PHP }},
		{name: "MariaDB", get: func(s store.Sample) string { return s.MariaDB }},
		{name: "nginx", get: func(s store.Sample) string { return s.Nginx }},
	}
	var out []versionChange
	for _, s := range samples {
		for _, t := range ts {
			v := t.get(s)
			if v == "" {
				continue
			}
			if t.prev != "" && v != t.prev {
				out = append(out, versionChange{s.At, t.name, t.prev, v})
			}
			t.prev = v
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

type trend struct {
	XLabels [3]string
	Days    int
	Ranges  []rangeChoice
	Label   string // Zeitraum für die Überschrift, z. B. "24 Stunden"
	Charts  []chart
	Band    []bandSeg
	Changes []versionChange
	W, H    float64
	BandH   float64
}

// buildTrend rechnet die Trend-Ansicht. history ist neueste zuerst (so liefert sie der Store).
// Der Zeitraum beginnt immer bei jetzt minus days; fehlende frühere Werte bleiben leer, sonst täuscht eine kurze Historie einen Verlauf vor.
// isContao: Updates und Fehler gibt es nur bei Contao-Sites; Diagramme ohne Werte werden weggelassen.
func buildTrend(history []store.Sample, days int, now time.Time, isContao bool, cfg status.Config) trend {
	asc := make([]store.Sample, len(history))
	for i, s := range history {
		asc[len(history)-1-i] = s
	}
	from := now.AddDate(0, 0, -days)
	t := trend{Days: days, Label: fmt.Sprintf("%d Tage", days), Ranges: rangeChoices, Band: buildBand(asc, from, now, cfg.StaleAfter, now.AddDate(0, 0, -cfg.Retention.FullDays)), Changes: versionChanges(asc), W: chartW, H: chartH, BandH: bandH}
	if days == 1 {
		t.Label = "24 Stunden"
	}
	layout := "02.01."
	if now.Sub(from) <= 48*time.Hour {
		layout = "02.01. 15:04"
	}
	t.XLabels = [3]string{from.Local().Format(layout), from.Add(now.Sub(from) / 2).Local().Format(layout), now.Local().Format(layout)}
	for _, m := range metrics {
		if m.contao && !isContao {
			continue
		}
		if c := buildChart(m, asc, from, now); !c.Empty {
			t.Charts = append(t.Charts, c)
		}
	}
	return t
}
