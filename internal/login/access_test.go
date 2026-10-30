package login

import "testing"

func TestAccess(t *testing.T) {
	a := Access{Subjects: []string{"u1"}, Emails: []string{"Me@Example.org"}, Groups: []string{"cockpit"}}
	cases := []struct {
		name string
		id   Identity
		want bool
	}{
		{"Subject", Identity{Subject: "u1"}, true},
		{"E-Mail ohne Groß/Klein", Identity{Subject: "x", Email: "me@example.org"}, true},
		{"Gruppe", Identity{Subject: "x", Groups: []string{"andere", "cockpit"}}, true},
		{"kein Treffer", Identity{Subject: "x", Email: "o@example.org", Groups: []string{"andere"}}, false},
		{"ohne Subject", Identity{Groups: []string{"cockpit"}}, false},
	}
	for _, c := range cases {
		if got := a.Allows(c.id); got != c.want {
			t.Errorf("%s: %v, erwartet %v", c.name, got, c.want)
		}
	}
	if (Access{}).Allows(Identity{Subject: "u1"}) {
		t.Error("leere Regel lässt jemanden durch")
	}
	if (Access{}).Validate() == nil || a.Validate() != nil {
		t.Error("Validate")
	}
}
