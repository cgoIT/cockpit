package sign

import (
	"crypto/ed25519"
	"path/filepath"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	priv, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "k", "signing.key"))
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	env, err := Config(priv, "server1", now, map[string]int{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Verify(pub, env, "server1", now.Add(time.Hour), time.Time{})
	if err != nil || string(p.Config) != `{"x":1}` {
		t.Fatalf("gültig: %v %s", err, p.Config)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	tampered := env
	tampered.Payload = append([]byte(nil), env.Payload...)
	tampered.Payload[len(tampered.Payload)-3] ^= 1
	cases := map[string]func() error{
		"fremder Schlüssel":    func() error { _, e := Verify(other, env, "server1", now, time.Time{}); return e },
		"verändert":            func() error { _, e := Verify(pub, tampered, "server1", now, time.Time{}); return e },
		"anderer Agent":        func() error { _, e := Verify(pub, env, "anderer", now, time.Time{}); return e },
		"abgelaufen":           func() error { _, e := Verify(pub, env, "server1", now.Add(TTL+time.Second), time.Time{}); return e },
		"älter als akzeptiert": func() error { _, e := Verify(pub, env, "server1", now, now.Add(time.Minute)); return e },
		"Zukunft":              func() error { _, e := Verify(pub, env, "server1", now.Add(-time.Hour), time.Time{}); return e },
	}
	for name, f := range cases {
		if f() == nil {
			t.Errorf("%s: muss abgelehnt werden", name)
		}
	}
}

func TestKeyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	a, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateKey(path)
	if err != nil || !a.Equal(b) {
		t.Fatalf("Schlüssel muss gleich bleiben: %v", err)
	}
	if _, err := ParsePublicKey("kaputt"); err == nil {
		t.Error("ungültiger öffentlicher Schlüssel")
	}
}

func TestVerifyJob(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	p := JobPayload{ID: "j", Agent: "a", Site: "s", Action: "maintenance-on", IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	good, _ := Job(priv, p)
	cfg, _ := Config(priv, "a", now, map[string]string{})
	mod := func(f func(*JobPayload)) Envelope { q := p; f(&q); e, _ := Job(priv, q); return e }
	for name, c := range map[string]struct {
		env   Envelope
		agent string
		at    time.Time
		ok    bool
	}{
		"gültig":            {good, "a", now, true},
		"fremder Schlüssel": {func() Envelope { e, _ := Job(other, p); return e }(), "a", now, false},
		"Konfiguration":     {cfg, "a", now, false}, // gleiche Schlüssel, anderes Präfix
		"anderer Agent":     {good, "b", now, false},
		"abgelaufen":        {good, "a", now.Add(10 * time.Minute), false},
		"in der Zukunft":    {mod(func(q *JobPayload) { q.IssuedAt = now.Add(time.Hour) }), "a", now, false},
		"ohne ID":           {mod(func(q *JobPayload) { q.ID = "" }), "a", now, false},
		"ohne Aktion":       {mod(func(q *JobPayload) { q.Action = "" }), "a", now, false},
		"Inhalt verändert":  {Envelope{Payload: append([]byte(nil), good.Payload...), Sig: good.Sig}, "a", now, true},
	} {
		if name == "Inhalt verändert" {
			c.env.Payload[5] ^= 1
			c.ok = false
		}
		if _, err := VerifyJob(pub, c.env, c.agent, c.at); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}
