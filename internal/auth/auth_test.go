package auth

import (
	"path/filepath"
	"testing"
)

func TestTokens(t *testing.T) {
	tok, hash, err := NewToken()
	if err != nil || len(tok) != 64 || hash != Hash(tok) {
		t.Fatalf("NewToken: %v", err)
	}
	if !Match(hash, tok) || Match(hash, tok+"x") || Match(hash, "") || Match("kein-hex", tok) {
		t.Error("Match")
	}
	p := filepath.Join(t.TempDir(), "tokens.json")
	got, err := Add(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := Load(p)
	if !Check(m, "a", got) || Check(m, "b", got) || len(Sites(m)) != 1 {
		t.Error("Check/Sites")
	}
}

func TestRemove(t *testing.T) {
	p := t.TempDir() + "/tokens.json"
	if ok, err := Remove(p, "a"); ok || err != nil {
		t.Fatalf("ohne Datei: %v %v", ok, err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := Add(p, id); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := Remove(p, "a"); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	m, _ := Load(p)
	if len(m) != 1 || m["b"] == "" {
		t.Errorf("nur a darf weg: %v", m)
	}
}
