# Projektkontext für Claude Code

Sitewatch zur Überwachung und Wartung von Contao-/PHP-Webseiten (Eigentümer: Software-Architekt, Go- und Contao-erfahren).
Sprache der UI, Texte und Kommentare: Deutsch. Code-Bezeichner: Englisch bzw. wie im vorhandenen Code.

## Architekturentscheidungen (nicht ohne Rückfrage ändern)
- **Nur ausgehende Verbindungen von den Agenten.** Auf Zielservern wird kein Port geöffnet. Reports werden gepusht; Aufträge holt der Agent später per Polling ab (optional später WebSocket mit Polling-Fallback).
- Agent in Go, ein statisches Binary pro Server. PHP-Bundle/Cron-Skript nur für Shared Hosting, gleiches JSON-Schema (`internal/report`).
- Sitewatch in Go, server-gerenderte Oberfläche (html/template, kein SPA). Ziel-Datenbank: SQLite hinter dem Interface `store.Store`.
- **Zuerst lesend.** Schreibende Aktionen (Wartungsmodus, Updates) erst nach Sicherheitskonzept: feste Aktionsliste, keine freie Shell, signierte Aufträge (Ed25519) mit Ablaufzeit, Freigabe im Dashboard.
- Tokens nie im Klartext speichern (nur SHA-256-Hash). Sitewatch ist das einzige eingehend erreichbare Ziel und muss entsprechend gehärtet werden.

## Konventionen
- Möglichst Standardbibliothek; neue Abhängigkeiten nur mit Grund (SQLite-Treiber ist der erste Kandidat).
- `go vet ./... && go test ./...` müssen grün sein, `gofmt` sauber.
- Alles in `testdata/` und `scripts/demo.sh` sind Beispieldaten/Stubs und als solche zu kennzeichnen.
- Fehlende Daten sind `nil`/leer, nie geraten. Was der Agent nicht ermitteln konnte, steht in `site.problems`.

## Offene Entscheidungen
- Pflichtfelder im Report, wenn nur das PHP-Bundle liefert (`host`-Daten fehlen).
- Aufbewahrungsdauer der Historie.
- Zugang zu Sitewatch: Passkey oder TOTP, IP-Beschränkung, mTLS.
Weitere Schritte: siehe `docs-intern/roadmap.md` (lokal, nicht im Repo) und README.md.
