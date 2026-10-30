package web

import (
	"fmt"
	"sort"
	"time"

	"sitewatch/internal/status"
	"sitewatch/internal/store"
)

// cell ist ein Feld der Matrix. Text "–" = kein Wert vorhanden; Level färbt nur, wo es eine Schwelle gibt.
type cell struct {
	Text, Level string
	Trend       *arrow
}

type matrixRow struct {
	ID, Name string
	Cells    []cell
}

type matrix struct {
	Title string
	Heads []string
	Rows  []matrixRow
}

// matrices liefert die Versions-Matrix und die Parameter-Matrix über alle Sites/Hosts, auch solche ohne Meldung.
func (s *Server) matrices(base map[string]store.Sample) (versions, params matrix, err error) {
	entries, err := s.Store.Latest()
	if err != nil {
		return
	}
	ids, err := s.siteIDs()
	if err != nil {
		return
	}
	versions, params = buildMatrices(entries, ids, s.Now(), s.Status, base)
	return
}

func buildMatrices(entries []store.Entry, ids []string, now time.Time, cfg status.Config, base map[string]store.Sample) (versions, params matrix) {
	versions = matrix{Title: "Versionen", Heads: []string{"Contao", "PHP", "MariaDB", "Nginx"}}
	params = matrix{Title: "Parameter", Heads: []string{"Platz frei", "Letztes Backup", "TLS läuft ab", "Cron", "Migrationen"}}
	seen := map[string]bool{}
	for _, e := range entries {
		r := e.Report
		seen[r.Site.ID] = true
		name := r.Site.Name
		if name == "" {
			name = r.Site.ID
		}
		disk := plain("")
		if p := r.Host.DiskFreePct; p != nil {
			b, ok := base[r.Site.ID]
			disk = cell{Text: fmt.Sprintf("%d %%", *p), Level: status.DiskLevel(*p, cfg).String(), Trend: diskArrow(p, b, ok, now)}
		}
		backup, tls := plain(""), plain("")
		if t := r.Site.LastBackup; t != nil {
			backup = cell{Text: status.Ago(now.Sub(*t)), Level: status.BackupLevel(*t, now, cfg).String()}
		}
		if t := r.Site.TLSExpires; t != nil {
			tls = cell{Text: t.Local().Format("02.01.2006"), Level: status.TLSLevel(*t, now, cfg).String()}
		}
		cron, mig := plain(""), plain("")
		if t := r.Site.CronLastRun; t != nil {
			cron = cell{Text: status.Ago(now.Sub(*t)), Level: status.CronLevel(*t, now, cfg).String()}
		}
		if n := r.Site.PendingMigrations; n != nil {
			mig = plain(fmt.Sprint(*n))
			if *n > 0 {
				mig.Level = status.Crit.String()
			}
		}
		versions.Rows = append(versions.Rows, matrixRow{r.Site.ID, name,
			[]cell{eolCell("contao", r.Site.Contao, now, cfg), eolCell("php", r.Site.PHP, now, cfg),
				eolCell("mariadb", r.Host.MariaDB, now, cfg), eolCell("nginx", r.Host.Nginx, now, cfg)}})
		params.Rows = append(params.Rows, matrixRow{r.Site.ID, name, []cell{disk, backup, tls, cron, mig}})
	}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		versions.Rows = append(versions.Rows, matrixRow{id, id, make([]cell, len(versions.Heads))})
		params.Rows = append(params.Rows, matrixRow{id, id, make([]cell, len(params.Heads))})
	}
	for _, m := range []*matrix{&versions, &params} {
		sort.SliceStable(m.Rows, func(i, j int) bool { return m.Rows[i].Name < m.Rows[j].Name })
		for _, r := range m.Rows {
			for i, c := range r.Cells {
				if c.Text == "" {
					r.Cells[i].Text = "–"
				}
			}
		}
	}
	return
}

func plain(text string) cell { return cell{Text: text, Level: "ok"} }

// eolCell zeigt die Version und färbt, wenn deren Support endet oder beendet ist.
func eolCell(product, version string, now time.Time, cfg status.Config) cell {
	l, _ := status.EOLLevel(product, version, now, cfg)
	return cell{Text: version, Level: l.String()}
}
