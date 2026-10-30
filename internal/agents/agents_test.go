package agents

import (
	"path/filepath"
	"testing"
	"time"

	"sitewatch/internal/agentcfg"
)

func site(id string) agentcfg.Site { return agentcfg.Site{ID: id, Kind: "php", Path: "/var/www/" + id} }

func TestLifecycle(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "agents.json"))
	now := time.Now()
	tok, err := s.Create("server1", now)
	if err != nil || tok == "" {
		t.Fatal(err)
	}
	if _, err := s.Create("server1", now); err == nil {
		t.Error("doppelte ID")
	}
	if _, err := s.Create("../x", now); err == nil {
		t.Error("ungültige ID")
	}
	if a, ok, _ := s.ByToken(tok); !ok || a.ID != "server1" {
		t.Fatal("Token muss passen")
	}
	if a, _ := s.Get("server1"); a.TokenHash == tok || a.TokenHash == "" {
		t.Error("nur der Hash wird gespeichert")
	}
	if err := s.Save("server1", agentcfg.Remote{Sites: []agentcfg.Site{site("a"), site("b")}}, now); err != nil {
		t.Fatal(err)
	}
	other, _ := s.Create("anderer", now)
	if err := s.Save("anderer", agentcfg.Remote{Sites: []agentcfg.Site{site("b")}}, now); err == nil {
		t.Error("Site b gehört schon zu server1")
	}
	if err := s.Save("anderer", agentcfg.Remote{Sites: []agentcfg.Site{{ID: "x", Kind: "php"}}}, now); err == nil {
		t.Error("ungültige Konfiguration (path fehlt)")
	}
	if err := s.Save("gibtsnicht", agentcfg.Remote{}, now); err != ErrNotFound {
		t.Errorf("unbekannter Agent: %v", err)
	}
	if ids, _ := s.SiteIDs(); len(ids) != 2 {
		t.Errorf("SiteIDs = %v", ids)
	}
	nt, err := s.Rotate("server1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ByToken(tok); ok {
		t.Error("altes Token muss ungültig sein")
	}
	if a, ok, _ := s.ByToken(nt); !ok || a.ID != "server1" {
		t.Error("neues Token")
	}
	if a, ok, _ := s.ByToken(other); !ok || a.ID != "anderer" {
		t.Error("anderes Token bleibt gültig")
	}
	if _, ok, _ := s.ByToken(""); ok {
		t.Error("leeres Token")
	}
	if err := s.Delete("server1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("server1"); err != ErrNotFound {
		t.Errorf("gelöscht: %v", err)
	}
}
