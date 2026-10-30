package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sitewatch/internal/agentcfg"
	"sitewatch/internal/sign"
)

const (
	cacheName  = "config.json"
	issuedName = "last_issued"
)

func readIssued(path string) time.Time {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return t
}

func writeIssued(path string, t time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(t.UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
}

// runRemote holt vor jedem Lauf die signierte Konfiguration bei Sitewatch (Sites und Intervall) und sammelt danach.
func runRemote(ctx context.Context, client *http.Client, bs agentcfg.Bootstrap, once, dryRun bool) {
	cur := &current{}
	kick := make(chan string, 8)
	if !once && !dryRun && len(bs.Actions) > 0 {
		jr := newJobRunner(bs, cur, kick)
		jr.refresh = func(ctx context.Context) {
			if rem, err := loadRemote(ctx, client, bs, time.Now()); err == nil {
				cur.set(bs.Config(rem))
			}
		}
		go jr.loop(ctx)
	}
	for {
		wait := time.Minute // ohne gültige Konfiguration bald erneut versuchen
		var js []job
		if rem, err := loadRemote(ctx, client, bs, time.Now()); err != nil {
			log.Printf("keine gültige Konfiguration, es wird nichts gesammelt: %v", err)
		} else {
			c := bs.Config(rem)
			cur.set(c)
			wait = c.Interval
			if len(c.Sites) == 0 {
				log.Print("Hinweis: dem Agenten sind in Sitewatch noch keine Sites zugewiesen")
			}
			// shortcut: Sites laufen nacheinander (bis 5 Minuten je Site); bei Bedarf parallelisieren.
			js = jobsFrom(c)
			for _, j := range js {
				if ctx.Err() != nil {
					return
				}
				runJob(ctx, client, bs.Sitewatch, j, dryRun)
			}
		}
		if once {
			return
		}
		timer := time.NewTimer(wait)
	waiting:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				break waiting
			case id := <-kick: // nach einem Auftrag nur diese Site sofort neu melden
				for _, j := range js {
					if j.cfg.SiteID == id {
						runJob(ctx, client, bs.Sitewatch, j, dryRun)
					}
				}
			}
		}
	}
}

var remoteMu sync.Mutex

// loadRemote liefert die Konfiguration: frisch von Sitewatch (geprüft und zwischengespeichert) oder, wenn das nicht geht,
// die zwischengespeicherte, solange sie gültig ist. Eine Antwort, die die Prüfung nicht besteht, ersetzt nie den Zwischenspeicher.
func loadRemote(ctx context.Context, c *http.Client, bs agentcfg.Bootstrap, now time.Time) (agentcfg.Remote, error) {
	remoteMu.Lock() // der Auftragskanal holt sie ebenfalls; die Dateien im State-Verzeichnis nicht gleichzeitig schreiben
	defer remoteMu.Unlock()
	cacheFile := filepath.Join(bs.State, cacheName)
	cached, haveCache := readEnvelope(cacheFile)
	var cachedIssued time.Time
	if haveCache {
		if p, err := sign.Verify(bs.PublicKey, cached, bs.Agent, now, time.Time{}); err == nil {
			cachedIssued = p.IssuedAt
		} else {
			log.Printf("zwischengespeicherte Konfiguration unbrauchbar: %v", err)
			haveCache = false
		}
	}
	// Die höchste akzeptierte Ausstellungszeit steht getrennt vom Zwischenspeicher, damit sie ein Ablauf oder Löschen nicht vergisst.
	if t := readIssued(filepath.Join(bs.State, issuedName)); t.After(cachedIssued) {
		cachedIssued = t
	}
	env, err := fetchEnvelope(ctx, c, bs)
	if err == nil {
		var p sign.Payload
		if p, err = sign.Verify(bs.PublicKey, env, bs.Agent, now, cachedIssued); err == nil {
			var rem agentcfg.Remote
			if rem, err = decodeRemote(p, bs); err == nil {
				if werr := writeIssued(filepath.Join(bs.State, issuedName), p.IssuedAt); werr != nil {
					log.Printf("Ausstellungszeit nicht gespeichert: %v", werr)
				}
				if werr := writeEnvelope(cacheFile, env); werr != nil {
					log.Printf("Konfiguration nicht zwischengespeichert: %v", werr)
				}
				return rem, nil
			}
		}
	}
	log.Printf("Konfiguration von Sitewatch nicht übernommen: %v", err)
	if haveCache {
		p, _ := sign.Verify(bs.PublicKey, cached, bs.Agent, now, time.Time{})
		if rem, derr := decodeRemote(p, bs); derr == nil {
			log.Print("verwende die zwischengespeicherte Konfiguration")
			return rem, nil
		}
	}
	return agentcfg.Remote{}, err
}

// decodeRemote liest die Konfiguration aus dem geprüften Inhalt und prüft sie noch einmal beim Agenten (Pfade gegen roots).
// Unbekannte Felder werden ignoriert, damit ein neueres Sitewatch einen älteren Agenten nicht aussperrt.
func decodeRemote(p sign.Payload, bs agentcfg.Bootstrap) (agentcfg.Remote, error) {
	var rem agentcfg.Remote
	if err := json.Unmarshal(p.Config, &rem); err != nil {
		return rem, err
	}
	return rem, rem.Validate(bs.Roots)
}

func fetchEnvelope(ctx context.Context, c *http.Client, bs agentcfg.Bootstrap) (sign.Envelope, error) {
	var env sign.Envelope
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(bs.Sitewatch, "/")+"/api/v1/agent/config", nil)
	if err != nil {
		return env, err
	}
	req.Header.Set("Authorization", "Bearer "+bs.Token)
	resp, err := c.Do(req)
	if err != nil {
		return env, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return env, fmt.Errorf("Sitewatch antwortet %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return env, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env)
}

func readEnvelope(path string) (sign.Envelope, bool) {
	var env sign.Envelope
	b, err := os.ReadFile(path)
	return env, err == nil && json.Unmarshal(b, &env) == nil
}

func writeEnvelope(path string, env sign.Envelope) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
