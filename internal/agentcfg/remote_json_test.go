package agentcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSiteJSONOmitsEmpty(t *testing.T) {
	no := false
	b, _ := json.Marshal(Site{ID: "x", Kind: "host", Path: "/p", Nginx: &no})
	if got := string(b); strings.Contains(got, `"url"`) || strings.Contains(got, `"token"`) || !strings.Contains(got, `"nginx":false`) {
		t.Errorf("%s", got)
	}
}
