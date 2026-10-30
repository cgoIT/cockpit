// Package agents verwaltet die Agenten (je Host einer) mit Token-Hash und der Konfiguration ihrer Sites.
// Ein Agent-Token darf für alle Sites melden, die in der Konfiguration des Agenten stehen.
package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"sitewatch/internal/agentcfg"
	"sitewatch/internal/auth"
	"sitewatch/internal/report"
)

type Agent struct {
	ID        string          `json:"id"`
	TokenHash string          `json:"token_hash"`
	Config    agentcfg.Remote `json:"config"`
	Updated   time.Time       `json:"updated"`
}

// Store ist eine kleine JSON-Datei (chmod 600). shortcut: die Datei wird bei jedem Zugriff gelesen und vollständig geschrieben;
// für wenige Agenten genügt das, bei vielen in den SQLite-Store verlagern.
type Store struct {
	path string
	mu   sync.Mutex
}

func Open(path string) *Store { return &Store{path: path} }

var ErrNotFound = errors.New("Agent nicht gefunden")

func (s *Store) load() ([]Agent, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var as []Agent
	return as, json.Unmarshal(b, &as)
}

// shortcut: ohne fsync; nach einem Absturz kann die Datei leer sein (load meldet dann einen Fehler), bei Bedarf Datei und Verzeichnis syncen.
func (s *Store) save(as []Agent) error {
	sort.Slice(as, func(i, j int) bool { return as[i].ID < as[j].ID })
	b, err := json.MarshalIndent(as, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// List liefert alle Agenten nach ID sortiert.
func (s *Store) List() ([]Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	as, err := s.load()
	sort.Slice(as, func(i, j int) bool { return as[i].ID < as[j].ID })
	return as, err
}

func (s *Store) Get(id string) (Agent, error) {
	as, err := s.List()
	for _, a := range as {
		if a.ID == id {
			return a, nil
		}
	}
	if err == nil {
		err = ErrNotFound
	}
	return Agent{}, err
}

// Create legt einen Agenten ohne Sites an und gibt das Token einmalig zurück.
func (s *Store) Create(id string, now time.Time) (string, error) {
	if !report.ValidID(id) {
		return "", errors.New("ungültige ID (erlaubt: a-z, 0-9, -)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	as, err := s.load()
	if err != nil {
		return "", err
	}
	for _, a := range as {
		if a.ID == id {
			return "", fmt.Errorf("Agent %q gibt es schon", id)
		}
	}
	tok, hash, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	if err := s.save(append(as, Agent{ID: id, TokenHash: hash, Updated: now.UTC()})); err != nil {
		return "", err
	}
	return tok, nil
}

// Save ersetzt die Konfiguration. Eine Site-ID darf nur zu einem Agenten gehören, sonst könnte ein Agent für fremde Sites melden.
func (s *Store) Save(id string, cfg agentcfg.Remote, now time.Time) error {
	if err := cfg.Validate(nil); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	as, err := s.load()
	if err != nil {
		return err
	}
	idx := -1
	for i, a := range as {
		if a.ID == id {
			idx = i
			continue
		}
		for _, o := range a.Config.Sites {
			for _, n := range cfg.Sites {
				if o.ID == n.ID {
					return fmt.Errorf("Site %q gehört schon zum Agenten %q", n.ID, a.ID)
				}
			}
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	as[idx].Config, as[idx].Updated = cfg, now.UTC()
	return s.save(as)
}

// Rotate ersetzt das Token; das alte wird sofort ungültig.
func (s *Store) Rotate(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	as, err := s.load()
	if err != nil {
		return "", err
	}
	for i := range as {
		if as[i].ID == id {
			tok, hash, err := auth.NewToken()
			if err != nil {
				return "", err
			}
			as[i].TokenHash = hash
			return tok, s.save(as)
		}
	}
	return "", ErrNotFound
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	as, err := s.load()
	if err != nil {
		return err
	}
	for i := range as {
		if as[i].ID == id {
			return s.save(append(as[:i], as[i+1:]...))
		}
	}
	return ErrNotFound
}

// ByToken sucht den Agenten zum Token. Es werden alle Hashes verglichen, damit die Dauer nicht verrät, ob es einen Treffer gab.
func (s *Store) ByToken(token string) (Agent, bool, error) {
	as, err := s.List()
	var found Agent
	ok := false
	for _, a := range as {
		if auth.Match(a.TokenHash, token) {
			found, ok = a, true
		}
	}
	return found, ok, err
}

// SiteIDs liefert die Sites aller Agenten.
func (s *Store) SiteIDs() ([]string, error) {
	as, err := s.List()
	var out []string
	for _, a := range as {
		for _, st := range a.Config.Sites {
			out = append(out, st.ID)
		}
	}
	return out, err
}
