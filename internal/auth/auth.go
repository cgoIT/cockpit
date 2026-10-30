// Package auth verwaltet die Tokens der Agenten. Gespeichert werden nur SHA-256-Hashes.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"

	"sitewatch/internal/report"
)

// Load liest die Datei (Site-ID -> Hash). Eine fehlende Datei ergibt eine leere Liste.
func Load(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Sites liefert die registrierten Site-IDs, sortiert.
func Sites(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Check prüft, ob token zur Site gehört (zeitkonstanter Vergleich).
func Check(m map[string]string, siteID, token string) bool {
	want, ok := m[siteID]
	if !ok || token == "" {
		return false
	}
	return Match(want, token)
}

// NewToken erzeugt ein zufälliges Token und den Hash, der gespeichert wird.
func NewToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(raw)
	return token, Hash(token), nil
}

// Hash ist der SHA-256 des Tokens in Hex; nur er wird gespeichert.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Match prüft, ob token zum gespeicherten Hash passt (zeitkonstanter Vergleich).
func Match(hash, token string) bool {
	want, err := hex.DecodeString(hash)
	if err != nil || token == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// Add erzeugt ein neues Token für die Site, speichert den Hash und gibt das Token einmalig zurück.
func Add(path, siteID string) (string, error) {
	if !report.ValidID(siteID) {
		return "", errors.New("ungültige Site-ID (erlaubt: a-z, 0-9, -)")
	}
	m, err := Load(path)
	if err != nil {
		return "", err
	}
	token, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	m[siteID] = hash
	return token, save(path, m)
}

// Remove löscht das Token der Site; false, wenn sie keines hatte. Der Agent mit diesem Token bekommt danach 401.
func Remove(path, siteID string) (bool, error) {
	m, err := Load(path)
	if err != nil {
		return false, err
	}
	if _, ok := m[siteID]; !ok {
		return false, nil
	}
	delete(m, siteID)
	return true, save(path, m)
}

func save(path string, m map[string]string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
