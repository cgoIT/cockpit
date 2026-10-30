package jobs

import "testing"

func TestActionsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range Actions {
		switch {
		case a.Name == "" || a.Label == "" || seen[a.Name]:
			t.Errorf("Name/Label fehlt oder doppelt: %+v", a)
		case (len(a.Site) == 0) == (len(a.Console) == 0):
			t.Errorf("%s: genau eines von Site und Console", a.Name)
		case len(a.Kinds) == 0:
			t.Errorf("%s: ohne Site-Arten", a.Name)
		case a.Limit() > MaxTimeout:
			t.Errorf("%s: Zeitlimit über MaxTimeout", a.Name)
		}
		seen[a.Name] = true
	}
	if a, ok := Find("maintenance-on"); !ok || !a.NeedsID() || !a.AllowsKind("contao") || a.AllowsKind("php") {
		t.Errorf("maintenance-on: %+v", a)
	}
}
