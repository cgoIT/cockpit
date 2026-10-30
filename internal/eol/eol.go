// Package eol holt Support-Enden (PHP, Contao, MariaDB, nginx) von endoflife.date und hält sie zwischengespeichert vor.
// Das Cockpit ruft die feste URL ab; schlägt der Abruf fehl, bleiben die letzten Daten gültig (und der Rückfall in status).
package eol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Products sind die abgefragten Produkte (Namen bei endoflife.date).
var Products = []string{"php", "contao", "mariadb", "nginx"}

const baseURL = "https://endoflife.date/api/v1/products/"

const maxBody = 4 << 20 // Antworten sind klein; das Limit schützt vor einer defekten oder manipulierten Quelle

// Provider liefert das Support-Ende je Produkt und Minor-Version ("8.5", "5.7", "11.4", "1.30").
type Provider struct {
	CacheFile string       // optional: Zwischenspeicher, damit nach einem Neustart sofort Daten da sind
	BaseURL   string       // nur für Tests; leer = endoflife.date
	Client    *http.Client // nil = Client mit Timeout

	mu   sync.RWMutex
	data map[string]map[string]time.Time // Produkt -> Minor -> eolFrom
}

// EOL gibt das Ende der Sicherheitsunterstützung. ok=false: unbekannt oder (wie bei aktuellem nginx) kein Datum vorgesehen.
func (p *Provider) EOL(product, version string) (time.Time, bool) {
	if p == nil {
		return time.Time{}, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	t, ok := p.data[product][Minor(version)]
	return t, ok
}

// Minor reduziert "5.7.15" auf "5.7".
func Minor(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// Load liest den Zwischenspeicher. Eine fehlende Datei ist kein Fehler.
func (p *Provider) Load() error {
	if p.CacheFile == "" {
		return nil
	}
	b, err := os.ReadFile(p.CacheFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var d map[string]map[string]time.Time
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("%s: %w", p.CacheFile, err)
	}
	p.mu.Lock()
	p.data = d
	p.mu.Unlock()
	return nil
}

// Refresh ruft alle Produkte ab. Es werden nur vollständig gelesene Produkte übernommen; ein Fehler bei einem Produkt
// lässt dessen alte Daten stehen und die anderen trotzdem aktualisieren.
func (p *Provider) Refresh(ctx context.Context) error {
	var errs []error
	fresh := map[string]map[string]time.Time{}
	for _, prod := range Products {
		m, err := p.fetch(ctx, prod)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", prod, err))
			continue
		}
		fresh[prod] = m
	}
	if len(fresh) > 0 {
		p.mu.Lock()
		if p.data == nil {
			p.data = map[string]map[string]time.Time{}
		}
		for k, v := range fresh {
			p.data[k] = v
		}
		snapshot := p.data
		p.mu.Unlock()
		if err := p.save(snapshot); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Provider) save(d map[string]map[string]time.Time) error {
	if p.CacheFile == "" {
		return nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tmp := p.CacheFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.CacheFile)
}

func (p *Provider) fetch(ctx context.Context, product string) (map[string]time.Time, error) {
	base := p.BaseURL
	if base == "" {
		base = baseURL
	}
	cl := p.Client
	if cl == nil {
		cl = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+product+"/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Result struct {
			Releases []struct {
				Name    string  `json:"name"`
				EOLFrom *string `json:"eolFrom"` // null = kein Ende vorgesehen
			} `json:"releases"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&doc); err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for _, r := range doc.Result.Releases {
		if r.EOLFrom == nil || r.Name == "" {
			continue
		}
		t, err := time.Parse("2006-01-02", *r.EOLFrom)
		if err != nil {
			continue
		}
		out[r.Name] = t.Add(24*time.Hour - time.Second) // Support gilt bis zum Ende des Tages, wie die Tabelle in status
	}
	if len(out) == 0 {
		return nil, errors.New("keine Releases mit Datum gefunden") // lieber alte Daten behalten als eine leere Antwort übernehmen
	}
	return out, nil
}

// Run lädt den Zwischenspeicher, ruft sofort ab und wiederholt das im Intervall, bis ctx endet.
func (p *Provider) Run(ctx context.Context, every time.Duration) {
	if err := p.Load(); err != nil {
		log.Printf("eol: Zwischenspeicher: %v", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := p.Refresh(ctx); err != nil {
			log.Printf("eol: Abruf: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CachePath legt den Zwischenspeicher im Datenverzeichnis ab.
func CachePath(dataDir string) string { return filepath.Join(dataDir, "eol.json") }
