// Package jobs enthält die feste Liste der Aktionen, die Sitewatch einem Agenten auftragen darf.
// Es gibt keine freie Shell: Der Auftrag nennt nur den Namen; Programm und Argumente stehen hier und werden vom Agenten selbst gebaut.
package jobs

import "time"

// TTL ist die Gültigkeit eines Auftrags ab Freigabe. Kurz, damit ein nicht abgeholter Auftrag nicht später überraschend läuft.
const TTL = 10 * time.Minute

// DefaultTimeout ist das Zeitlimit einer Aktion, wenn sie kein eigenes hat.
const DefaultTimeout = 10 * time.Minute

// MaxTimeout ist das längste Zeitlimit; Sitewatch hält einen laufenden Auftrag danach für verwaist (siehe store.jobStale).
const MaxTimeout = 30 * time.Minute

// Level legt fest, wie Sitewatch die Freigabe verlangt.
type Level int

const (
	Direct  Level = iota // lesend: ein Klick in der Liste
	Confirm              // ändert etwas: Bestätigungsseite
	TypeID               // wirkt auf Besucher oder ist umfangreich: Bestätigungsseite, auf der die Site-ID abgetippt wird
)

// Action ist eine erlaubte Aktion. Entweder Site (Argumente für <SITE_DIR>/bin/site, das Werkzeug des Website-Stacks) oder
// Console (Argumente für vendor/bin/contao-console in der Site, über den php-Wrapper).
type Action struct {
	Name, Label string
	Site        []string
	Console     []string
	Kinds       []string      // für welche Site-Arten (contao, php)
	Timeout     time.Duration // 0 = DefaultTimeout
	Level       Level
}

// NeedsID: Die Bestätigungsseite verlangt, die Site-ID abzutippen.
func (a Action) NeedsID() bool { return a.Level == TypeID }

// Limit liefert das Zeitlimit der Aktion.
func (a Action) Limit() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return DefaultTimeout
}

// AllowsKind: die Aktion gilt für Sites dieser Art.
func (a Action) AllowsKind(kind string) bool {
	for _, k := range a.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

var (
	contao    = []string{"contao"}
	contaoPHP = []string{"contao", "php"}
)

// Actions ist die Liste; neue Aktionen kommen nur hier und in der lokal erlaubten "actions"-Liste des Agenten hinzu.
var Actions = []Action{
	{Name: "site-status", Label: "Site: Status", Site: []string{"status"}, Kinds: contaoPHP, Level: Direct},
	{Name: "site-doctor", Label: "Site: Doctor", Site: []string{"doctor"}, Kinds: contaoPHP, Level: Direct},
	{Name: "site-validate-config", Label: "Site: Konfiguration prüfen", Site: []string{"validate-config"}, Kinds: contaoPHP, Level: Direct},
	{Name: "contao-snapshot-list", Label: "Contao: Snapshots auflisten", Site: []string{"contao:snapshot:list"}, Kinds: contao, Level: Direct},
	{Name: "db-list", Label: "DB: Snapshots auflisten", Site: []string{"db:list"}, Kinds: contaoPHP, Level: Direct},

	{Name: "contao-cache-refresh", Label: "Contao: Cache erneuern", Site: []string{"contao:cache:refresh"}, Kinds: contao, Level: Confirm},
	{Name: "contao-filesync", Label: "Contao: Dateisync", Site: []string{"contao:filesync"}, Kinds: contao, Level: Confirm},
	{Name: "contao-snapshot", Label: "Contao: Snapshot anlegen", Site: []string{"contao:snapshot"}, Kinds: contao, Level: Confirm},
	{Name: "db-snapshot", Label: "DB: Snapshot anlegen", Site: []string{"db:snapshot"}, Kinds: contaoPHP, Level: Confirm},
	{Name: "site-restart", Label: "Site: Neustart", Site: []string{"restart"}, Kinds: contaoPHP, Level: Confirm},
	{Name: "maintenance-off", Label: "Contao: Wartungsmodus ausschalten", Console: []string{"contao:maintenance-mode", "disable"}, Kinds: contao, Level: Confirm},

	{Name: "maintenance-on", Label: "Contao: Wartungsmodus einschalten", Console: []string{"contao:maintenance-mode", "enable"}, Kinds: contao, Level: TypeID},
	{Name: "contao-update", Label: "Contao: Update (Snapshot, composer update, Migration)", Site: []string{"contao:update"}, Kinds: contao, Timeout: 25 * time.Minute, Level: TypeID},
}

// Find liefert die Aktion zum Namen.
func Find(name string) (Action, bool) {
	for _, a := range Actions {
		if a.Name == name {
			return a, true
		}
	}
	return Action{}, false
}
