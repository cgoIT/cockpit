package agentcfg

import (
	"crypto/ed25519"
	"os"
	"strings"
	"testing"

	"cockpit/internal/sign"
)

// Die Vorlage muss sich laden lassen, sobald die Platzhalter ersetzt sind (Schutz vor Drift zum Bootstrap-Format).
func TestBootstrapExampleLoads(t *testing.T) {
	b, err := os.ReadFile("../../scripts/systemd/cockpit-agent.bootstrap.example.json")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	s := strings.ReplaceAll(string(b), "AUSGABE-VON-cockpit-pubkey", sign.EncodePublicKey(pub))
	p := t.TempDir() + "/bootstrap.json"
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBootstrap(p); err != nil {
		t.Errorf("Vorlage passt nicht zum Format: %v", err)
	}
}

func TestBootstrapRejectsUnknownAction(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	p := t.TempDir() + "/bootstrap.json"
	body := `{"cockpit":"https://c.example","agent":"a","token":"t","public_key":"` + sign.EncodePublicKey(pub) + `","state":"/var/lib/x","roots":["/appl"],"actions":["rm-rf"]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBootstrap(p); err == nil || !strings.Contains(err.Error(), "actions") {
		t.Errorf("unbekannte Aktion muss abgelehnt werden: %v", err)
	}
}
