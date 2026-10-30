// Package sign signiert die Konfiguration, die das Cockpit an Agenten ausliefert (Ed25519).
// Der Agent kennt nur den öffentlichen Schlüssel (aus seiner lokalen Datei, nicht aus der Antwort des Cockpits) und prüft damit
// Signatur, Agent, Ablauf und dass die Konfiguration nicht älter ist als die zuletzt akzeptierte.
package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// prefix trennt diese Signaturen von anderen Verwendungen desselben Schlüssels (z. B. spätere Aufträge).
const prefix = "cockpit-agent-config-v1\n"

// TTL ist die Gültigkeit einer ausgelieferten Konfiguration; solange darf der Agent sie ohne Cockpit weiterverwenden.
const TTL = 30 * 24 * time.Hour

// maxSkew ist die erlaubte Uhrabweichung zwischen Cockpit und Agent.
const maxSkew = 10 * time.Minute

// Envelope ist die Antwort der Config-API: der signierte Inhalt (JSON von Payload) und die Signatur.
type Envelope struct {
	Payload []byte `json:"payload"`
	Sig     []byte `json:"sig"`
}

// Payload ist der signierte Inhalt.
type Payload struct {
	Agent     string          `json:"agent"`
	IssuedAt  time.Time       `json:"issued_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	Config    json.RawMessage `json:"config"`
}

// LoadOrCreateKey liest den privaten Schlüssel (Base64 des Seeds, chmod 600) oder legt ihn neu an.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) { // ein gleichzeitiger Start war schneller: dessen Schlüssel gilt
			return LoadOrCreateKey(path)
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		_, err = f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n")
		return priv, err
	}
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s ist für andere lesbar (Rechte %o), bitte chmod 600", path, fi.Mode().Perm())
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: kein gültiger Schlüssel", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// ParsePublicKey liest einen öffentlichen Schlüssel in Base64.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("öffentlicher Schlüssel ungültig (erwartet: Base64, 32 Byte)")
	}
	return ed25519.PublicKey(b), nil
}

// EncodePublicKey ist das Gegenstück zu ParsePublicKey.
func EncodePublicKey(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }

// Config signiert cfg für den Agenten agent, gültig ab now für TTL.
func Config(priv ed25519.PrivateKey, agent string, now time.Time, cfg any) (Envelope, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return Envelope{}, err
	}
	p, err := json.Marshal(Payload{Agent: agent, IssuedAt: now.UTC(), ExpiresAt: now.Add(TTL).UTC(), Config: raw})
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Payload: p, Sig: ed25519.Sign(priv, append([]byte(prefix), p...))}, nil
}

// Verify prüft die Signatur und liefert den Inhalt. Gelesen wird der Inhalt erst nach erfolgreicher Prüfung.
// agent: der erwartete Empfänger (verhindert, dass die Konfiguration eines anderen Agenten untergeschoben wird).
// notBefore: Ausstellungszeit der zuletzt akzeptierten Konfiguration; ältere werden abgelehnt (kein Zurückspielen).
func Verify(pub ed25519.PublicKey, env Envelope, agent string, now, notBefore time.Time) (Payload, error) {
	var p Payload
	if !ed25519.Verify(pub, append([]byte(prefix), env.Payload...), env.Sig) {
		return p, errors.New("Signatur ungültig")
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return p, err
	}
	switch {
	case p.Agent != agent:
		return p, fmt.Errorf("Konfiguration ist für %q, nicht für %q", p.Agent, agent)
	case !now.Before(p.ExpiresAt):
		return p, fmt.Errorf("Konfiguration abgelaufen seit %s", p.ExpiresAt.Local().Format("02.01.2006 15:04"))
	case p.IssuedAt.After(now.Add(maxSkew)):
		return p, errors.New("Konfiguration liegt in der Zukunft (Uhr prüfen)")
	case p.IssuedAt.Before(notBefore):
		return p, errors.New("Konfiguration ist älter als die zuletzt akzeptierte")
	}
	return p, nil
}

// jobPrefix trennt Aufträge von der Konfiguration: Eine Konfiguration lässt sich nie als Auftrag ausgeben und umgekehrt.
const jobPrefix = "cockpit-agent-job-v1\n"

// JobPayload ist ein signierter Auftrag. Er gilt für genau einen Agenten, eine Site und eine Aktion aus der festen Liste
// (internal/jobs); die ID verhindert, dass ein Auftrag zweimal läuft.
type JobPayload struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	Site      string    `json:"site"`
	Action    string    `json:"action"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Job signiert einen Auftrag.
func Job(priv ed25519.PrivateKey, p JobPayload) (Envelope, error) {
	p.IssuedAt, p.ExpiresAt = p.IssuedAt.UTC(), p.ExpiresAt.UTC()
	b, err := json.Marshal(p)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Payload: b, Sig: ed25519.Sign(priv, append([]byte(jobPrefix), b...))}, nil
}

// VerifyJob prüft Signatur, Empfänger, Ablauf und Uhrabweichung. Ob die ID neu ist, entscheidet der Aufrufer (er hat den Zustand).
func VerifyJob(pub ed25519.PublicKey, env Envelope, agent string, now time.Time) (JobPayload, error) {
	var p JobPayload
	if !ed25519.Verify(pub, append([]byte(jobPrefix), env.Payload...), env.Sig) {
		return p, errors.New("Signatur ungültig")
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return p, err
	}
	switch {
	case p.Agent != agent:
		return p, fmt.Errorf("Auftrag ist für %q, nicht für %q", p.Agent, agent)
	case !now.Before(p.ExpiresAt):
		return p, errors.New("Auftrag abgelaufen")
	case p.IssuedAt.After(now.Add(maxSkew)):
		return p, errors.New("Auftrag liegt in der Zukunft (Uhr prüfen)")
	case p.ID == "" || p.Site == "" || p.Action == "":
		return p, errors.New("Auftrag unvollständig")
	}
	return p, nil
}
