package web

import (
	"fmt"
	"math"
	"time"

	"sitewatch/internal/status"
	"sitewatch/internal/store"
)

// arrow ist ein kleiner Trendhinweis neben einem Wert: Pfeil mit Betrag der Veränderung, Tooltip mit Vergleichswert.
type arrow struct{ Text, Title, Class string }

const (
	trendWindow     = 7 * 24 * time.Hour // verglichen wird mit dem ältesten Punkt der letzten 7 Tage
	trendMinSpan    = 24 * time.Hour     // erst ab einem Tag Historie ist ein Pfeil aussagekräftig
	forecastMaxDays = 90                 // weiter als ein Quartal in die Zukunft zu rechnen wäre geraten
)

// trendArrow vergleicht cur mit base. Kein Pfeil ohne Vergleichswert, bei zu kurzer Historie oder zu kleiner Veränderung
// (Rauschen). goodWhenUp: ein Anstieg ist gut (freier Speicher), sonst schlecht (offene Updates).
func trendArrow(cur, base int, baseAt, now time.Time, goodWhenUp bool, minDelta int, format func(int) string) *arrow {
	span := now.Sub(baseAt)
	d := cur - base
	if span < trendMinSpan || d > -minDelta && d < minDelta {
		return nil
	}
	a := &arrow{Text: fmt.Sprintf("↓ %d", -d), Title: fmt.Sprintf("Vor %s: %s", status.Duration(span), format(base))}
	if d > 0 {
		a.Text = fmt.Sprintf("↑ %d", d)
	}
	a.Class = "bad"
	if (d > 0) == goodWhenUp {
		a.Class = "good"
	}
	return a
}

func percent(n int) string { return fmt.Sprintf("%d %%", n) }
func count(n int) string   { return fmt.Sprint(n) }

// diskArrow und updatesArrow: Veränderung gegenüber dem Vergleichspunkt base (ok=false: es gibt keinen).
func diskArrow(cur *int, base store.Sample, ok bool, now time.Time) *arrow {
	if !ok || cur == nil || base.DiskFreePct == nil {
		return nil
	}
	a := trendArrow(*cur, *base.DiskFreePct, base.At, now, true, 2, percent)
	if a != nil && *cur < *base.DiskFreePct {
		// shortcut: gerade Linie zwischen Vergleichspunkt und jetzt; saisonale Schwankungen und Aufräumen bleiben unberücksichtigt.
		perDay := float64(*base.DiskFreePct-*cur) / now.Sub(base.At).Hours() * 24
		if days := int(math.Ceil(float64(*cur) / perDay)); days <= forecastMaxDays {
			a.Text += fmt.Sprintf(" · voll in ~%d Tg.", days)
			a.Title += fmt.Sprintf("; bei gleichem Tempo ist der Platz in etwa %d Tagen voll", days)
		}
	}
	return a
}

func updatesArrow(cur int, base store.Sample, ok bool, now time.Time) *arrow {
	if !ok {
		return nil
	}
	return trendArrow(cur, base.OpenUpdates, base.At, now, false, 1, count)
}

// baselines lädt die Vergleichspunkte der letzten 7 Tage. Ein Fehler lässt nur die Pfeile weg, nicht die Seite.
func (s *Server) baselines() map[string]store.Sample {
	b, err := s.Store.Baselines(s.Now().Add(-trendWindow))
	if err != nil {
		return nil
	}
	return b
}
