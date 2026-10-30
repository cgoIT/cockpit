package agentcfg

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

const base = `{"cockpit":"https://c.example","interval":"2m","spool":"/var/spool/x","sites":[
 {"id":"host-a","kind":"host","token":"t1"},
 {"id":"web-a","kind":"contao","path":"/srv/web-a","env":{"SITE_DIR":"/srv/web-a-dir"},"token":"t2"}]}`

func TestLoadOK(t *testing.T) {
	c, err := Load(write(t, "c.json", base, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != 2*time.Minute || len(c.Sites) != 2 || c.Sites[1].Token != "t2" {
		t.Errorf("%+v", c)
	}
	if got := c.Sites[1].EnvList(); len(got) != 1 || got[0] != "SITE_DIR=/srv/web-a-dir" {
		t.Errorf("EnvList: %v", got)
	}
}

func TestTokenFile(t *testing.T) {
	tf := write(t, "tok", "geheim\n", 0o600)
	c, err := Load(write(t, "c.json", `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"host","token_file":"`+tf+`"}]}`, 0o600))
	if err != nil || c.Sites[0].Token != "geheim" || c.Interval != 5*time.Minute {
		t.Fatalf("%+v %v", c, err)
	}
	open := write(t, "tok2", "geheim", 0o644)
	if _, err := Load(write(t, "c2.json", `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"host","token_file":"`+open+`"}]}`, 0o600)); err == nil {
		t.Error("lesbare token_file akzeptiert")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"Rechte":          "",
		"unbekannter Key": `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"host","token":"t","tokn":"x"}]}`,
		"http":            `{"cockpit":"http://c.example","sites":[{"id":"h","kind":"host","token":"t"}]}`,
		"Intervall":       `{"cockpit":"https://c.example","interval":"1s","sites":[{"id":"h","kind":"host","token":"t"}]}`,
		"keine Sites":     `{"cockpit":"https://c.example","sites":[]}`,
		"doppelt":         `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"host","token":"t"},{"id":"h","kind":"host","token":"t"}]}`,
		"ID":              `{"cockpit":"https://c.example","sites":[{"id":"../x","kind":"host","token":"t"}]}`,
		"kind":            `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"foo","token":"t"}]}`,
		"path fehlt":      `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"php","token":"t"}]}`,
		"Token fehlt":     `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"host"}]}`,
		"relativer path":  `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"php","path":"rel","token":"t"}]}`,
		"relativer spool": `{"cockpit":"https://c.example","spool":"rel","sites":[{"id":"h","kind":"host","token":"t"}]}`,
		"gleiches Token":  `{"cockpit":"https://c.example","sites":[{"id":"a","kind":"host","token":"t"},{"id":"b","kind":"host","token":" t "}]}`,
		"Restdaten":       `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"host","token":"t"}]} {}`,
		"nginx true":      `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"host","token":"t","nginx":true}]}`,
		"env-Name":        `{"cockpit":"https://c.example","sites":[{"id":"h","kind":"host","token":"t","env":{"A=B":"x"}}]}`,
	}
	for name, content := range cases {
		mode := os.FileMode(0o600)
		if name == "Rechte" {
			content, mode = base, 0o644
		}
		if _, err := Load(write(t, "c.json", content, mode)); err == nil {
			t.Errorf("%s: kein Fehler", name)
		}
	}
}

func TestCheckCockpitURL(t *testing.T) {
	for _, u := range []string{"https://c.example", "http://127.0.0.1:8099", "http://localhost"} {
		if err := CheckCockpitURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"http://c.example", "http://localhost.evil.example", "ftp://x", "", "c.example"} {
		if err := CheckCockpitURL(u); err == nil {
			t.Errorf("%s akzeptiert", u)
		}
	}
}

func TestReadPrivateRejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	if _, err := readPrivate(dir); err == nil {
		t.Error("Verzeichnis akzeptiert")
	}
	open := write(t, "offen.json", "{}", 0o644)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(open, link); err != nil {
		t.Skip(err)
	}
	if _, err := readPrivate(link); err == nil {
		t.Error("Symlink auf lesbare Datei akzeptiert")
	}
}

func TestIgnoreUpdates(t *testing.T) {
	cfg := func(rules string) string {
		return `{"cockpit":"https://c.example","sites":[{"id":"w","kind":"contao","path":"/srv/w","token":"t","ignore_updates":` + rules + `}]}`
	}
	if c, err := Load(write(t, "ok.json", cfg(`["contao/*:6.*","a/b"]`), 0o600)); err != nil || len(c.Sites[0].IgnoreUpdates) != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{`["[x"]`, `[":6.*"]`, `["a/b:[1"]`, `["a/b:"]`} {
		if _, err := Load(write(t, "bad.json", cfg(bad), 0o600)); err == nil {
			t.Errorf("%s nicht abgelehnt", bad)
		}
	}
}

func TestRemoteValidate(t *testing.T) {
	ok := Site{ID: "a", Kind: "contao", Path: "/var/www/a", BackupDir: "/var/backups/a", Env: map[string]string{"SITE_DIR": "/var/www/a"}}
	if err := (Remote{Interval: "5m", Sites: []Site{ok}}).Validate([]string{"/var/www", "/var/backups"}); err != nil {
		t.Fatal(err)
	}
	bad := map[string]Remote{
		"Intervall zu kurz": {Interval: "1s"},
		"Token":             {Sites: []Site{{ID: "a", Kind: "php", Path: "/x", Token: "geheim"}}},
		"fremde env":        {Sites: []Site{{ID: "a", Kind: "php", Path: "/x", Env: map[string]string{"LD_PRELOAD": "/tmp/x.so"}}}},
		"doppelt":           {Sites: []Site{ok, ok}},
		"außerhalb":         {Sites: []Site{{ID: "a", Kind: "php", Path: "/etc"}}},
		"Präfix-Trick":      {Sites: []Site{{ID: "a", Kind: "php", Path: "/var/www-x/a"}}},
		"..":                {Sites: []Site{{ID: "a", Kind: "php", Path: "/var/www/../etc"}}},
		"SITE_DIR außen":    {Sites: []Site{{ID: "a", Kind: "php", Path: "/var/www/a", Env: map[string]string{"SITE_DIR": "/root"}}}},
	}
	for name, r := range bad {
		if r.Validate([]string{"/var/www", "/var/backups"}) == nil {
			t.Errorf("%s: muss abgelehnt werden", name)
		}
	}
	if (Remote{Sites: []Site{{ID: "a", Kind: "php", Path: "/etc"}}}).Validate(nil) != nil {
		t.Error("ohne roots keine Verzeichnisprüfung (Cockpit kennt sie nicht)")
	}
}

func TestSiteActionsAreChecked(t *testing.T) {
	site := func(kind string, acts ...string) Site { return Site{ID: "s", Kind: kind, Path: "/p", Actions: acts} }
	if err := checkSite(0, site("contao", "site-status", "contao-cache-refresh")); err != nil {
		t.Errorf("gültig: %v", err)
	}
	for name, s := range map[string]Site{
		"unbekannt":   site("contao", "rm-rf"),
		"falsche Art": site("php", "maintenance-on"),
		"doppelt":     site("contao", "site-status", "site-status"),
		"Host":        {ID: "h", Kind: "host", Actions: []string{"site-status"}},
	} {
		if err := checkSite(0, s); err == nil {
			t.Errorf("%s: muss abgelehnt werden", name)
		}
	}
}
