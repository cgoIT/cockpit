package login

import (
	"errors"
	"strings"
)

// Access legt fest, wer nach erfolgreichem Login hinein darf. Treffer in einer der Listen genügt.
// Eine leere Access lässt niemanden durch.
type Access struct {
	Subjects, Emails, Groups []string
}

// Validate lehnt eine leere Regel ab, damit ein Konfigurationsfehler nie "alle" bedeutet.
func (a Access) Validate() error {
	if len(a.Subjects)+len(a.Emails)+len(a.Groups) == 0 {
		return errors.New("keine Zugriffsregel (Benutzer, E-Mail oder Gruppe) konfiguriert")
	}
	return nil
}

// Allows prüft die Identität. E-Mails zählen nur, wenn der Provider sie als verifiziert gemeldet hat (dann ist Identity.Email gesetzt).
func (a Access) Allows(id Identity) bool {
	if id.Subject == "" {
		return false
	}
	if has(a.Subjects, id.Subject, false) {
		return true
	}
	if id.Email != "" && has(a.Emails, id.Email, true) {
		return true
	}
	for _, g := range id.Groups {
		if has(a.Groups, g, false) {
			return true
		}
	}
	return false
}

func has(list []string, v string, fold bool) bool {
	for _, x := range list {
		if x == v || (fold && strings.EqualFold(x, v)) {
			return true
		}
	}
	return false
}
