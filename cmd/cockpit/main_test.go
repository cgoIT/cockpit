package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cockpit/internal/agentcfg"
)

func TestConfigBlock(t *testing.T) {
	var got struct{ ID, Name, Kind, URL, Path, Token string }
	b := configBlock("beispiel-php", "", "php", "https://php.example", "/srv/beispiel-php", "tok")
	if err := json.Unmarshal([]byte(b), &got); err != nil {
		t.Fatalf("kein gültiges JSON: %v\n%s", err, b)
	}
	if got.ID != "beispiel-php" || got.Name != "beispiel-php" || got.Kind != "php" || got.Path != "/srv/beispiel-php" || got.Token != "tok" {
		t.Errorf("%+v", got)
	}
	if h := configBlock("h", "Host", "host", "", "", "t"); strings.Contains(h, "path") || strings.Contains(h, "url") {
		t.Errorf("host-Block enthält path/url:\n%s", h)
	}
	if c := configBlock("c", "", "contao", "", "", "t"); !strings.Contains(c, "<PFAD EINTRAGEN>") {
		t.Errorf("Platzhalter für den Pfad fehlt:\n%s", c)
	}
}

// Der Block muss in die Agent-Konfiguration passen, sonst driften Schema und Ausgabe auseinander.
func TestConfigBlockLoadsInAgentConfig(t *testing.T) {
	blocks := []string{
		configBlock("web", "Meine Web", "contao", "https://example.org", "/srv/web", "t1"),
		configBlock("host-a", "", "host", "", "", "t2"),
	}
	doc := `{"cockpit":"https://c.example","sites":[` + strings.Join(blocks, ",") + `]}`
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := agentcfg.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sites) != 2 || c.Sites[0].Name != "Meine Web" || c.Sites[0].Path != "/srv/web" {
		t.Errorf("%+v", c.Sites)
	}
	// Der Platzhalter muss vom Agent abgelehnt werden.
	bad := `{"cockpit":"https://c.example","sites":[` + configBlock("web", "", "contao", "", "", "t") + `]}`
	if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := agentcfg.Load(p); err == nil {
		t.Error("Platzhalter für den Pfad wurde akzeptiert")
	}
}
