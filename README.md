# Cockpit (Prototyp)

Management-Cockpit für Contao- und PHP-Webseiten. Agenten sammeln Zustandsdaten (Versionen,
Composer-Updates, Sicherheitshinweise, letzte Fehler) und **pushen sie ausgehend per HTTPS** an das
Cockpit. Auf den Zielservern wird kein Port geöffnet.

Stand: lesend und produktiv im Einsatz (Cockpit mit OIDC-Login, ein Agent auf dem Server „server1" für Host und Docker-Sites).
Schreibende Aktionen nur über die feste Liste der Aufträge (derzeit Wartungsmodus, siehe „Aufträge“); sonst nur lesend. Der Speicher ist SQLite (siehe „Speicher und Aufbewahrung"); Alarmierung per E-Mail ist umgesetzt.

## Schnell ausprobieren

```sh
./scripts/demo.sh          # baut, startet das Cockpit auf :8099, lässt Demo-Agenten laufen
# Browser: http://127.0.0.1:8099
kill $(cat /tmp/cockpit-demo/cockpit.pid)
go test ./...
```

Die Demo nutzt Beispieldaten und einen **composer-Stub** (`testdata/bin/composer`). Nichts davon sind echte Zahlen.

## Gegen eine echte Site

```sh
go build -o bin/ ./cmd/...
bin/cockpit token meine-site > token.txt        # Token einmalig anzeigen, Hash landet in tokens.json
# mit -kind/-name/-url/-path gibt `token` zusätzlich (auf stderr, nicht in Logs umleiten) den fertigen Block für die Agent-Konfiguration aus; die Datei muss `chmod 600` haben
bin/cockpit serve -addr 127.0.0.1:8080          # hinter nginx mit TLS betreiben

bin/agent -site-id meine-site -name "Meine Site" -kind contao -url https://example.org \
  -path /var/www/meine-site -cockpit https://cockpit.example -token "$(cat token.txt)" -once
# erst ohne Senden prüfen:  bin/agent ... -dry-run
```

`-spool <dir>` puffert Reports, wenn das Cockpit nicht erreichbar ist, und sendet sie beim nächsten Lauf nach.

## Betrieb mit Login (OIDC, z. B. Authentik)

```sh
export COCKPIT_OIDC_ISSUER=https://auth.example/application/o/cockpit/
export COCKPIT_OIDC_CLIENT_ID=... COCKPIT_OIDC_CLIENT_SECRET=...
export COCKPIT_OIDC_REDIRECT_URL=https://cockpit.example/callback
export COCKPIT_SESSION_KEY=$(openssl rand -hex 32)   # fest halten, sonst verfallen alle Sitzungen
bin/cockpit serve -allow-group cockpit -trusted-proxy 172.18.0.0/16   # oder -allow-sub / -allow-email
```

Nur `/api/` und `/healthz` sind ohne Login erreichbar (Agent-Token). Entwicklung ohne Login: `-no-login`.

Als Dienst: `scripts/systemd/cockpit.service` mit der Umgebungsdatei `scripts/systemd/cockpit.env.example` (die Einrichtungsschritte stehen am Anfang der Unit). Die Dateien im Datenverzeichnis (`cockpit.db`, `signing.key`, `agents.json`, `tokens.json`) gehören in die Sicherung; ohne `signing.key` müssten alle Agenten den neuen öffentlichen Schlüssel erhalten.

## Speicher und Aufbewahrung

Alles liegt in `<data>/cockpit.db` (SQLite via `modernc.org/sqlite`, reines Go, Rechte 0600, WAL). Sie enthält:
- `daily_reports`: je Site und Tag (UTC) der **letzte vollständige Report**; der jüngste ist zugleich der aktuelle. Ein nachgesendeter älterer Report überschreibt den Tag nicht.
- `samples`: der Verlauf als kleine Zeilen (Status, Speicher, offene Updates, Fehler, Migrationen, Alter von Cron und Backup, Versionen). Letzter Wert je Block; Status = schlimmster, Speicher = kleinster Wert im Block.
- `alert_state`: für welche Sites ein Alarm offen ist (ein Neustart löst keinen erneuten Alarm aus).

Standard: Verlauf 30 Tage jeder Punkt, bis 90 Tage ein Punkt je 6 Stunden, bis 365 Tage einer je Tag, danach gelöscht; Tagesstände 90 Tage (der jüngste je Site bleibt immer).
Einstellbar in der Schwellwert-Datei: `history_full_days`, `history_6h_days`, `history_max_days`, `history_reports_days`. Verdichtet wird beim Start und täglich.
Beim ersten Start wird `<id>.history.jsonl`/`<id>.latest.json` aus dem Datenverzeichnis einmal importiert; die Dateien bleiben liegen (danach löschbar).
Sicherung: die Datei `cockpit.db` (mit `sqlite3 cockpit.db ".backup ziel.db"`, nicht beim Schreiben kopieren; `-wal`/`-shm` gehören dazu).

## Schwellwerte

Standardwerte stehen in `status.DefaultConfig()`. Zum Ändern `cockpit serve -thresholds schwellen.json`; fehlende Felder behalten den Standard,
unbekannte Felder oder Werte unter 1 lassen den Start scheitern:

```json
{"stale_after_minutes": 15, "error_warn": 10, "eol_warn_days": 90, "tls_warn_days": 14,
 "backup_max_hours": 36, "disk_warn_pct": 15, "disk_crit_pct": 5,
 "cron_warn_minutes": 120, "cron_crit_minutes": 240, "maintenance_warn_minutes": 120, "maintenance_crit_minutes": 480,
 "history_full_days": 30, "history_6h_days": 90, "history_max_days": 365, "history_reports_days": 90}
```

`disk_*_pct` ist der freie Speicher in Prozent (Warnung bzw. kritisch darunter). `-stale` gilt weiter, die Datei hat Vorrang.

## Contao-Konsole (offene Migrationen, Cron)

Bei `kind: "contao"` ruft der Agent mit dem konfigurierten `php` (also auch über die Docker-Wrapper) `vendor/bin/contao-console` auf:
`contao:migrate --dry-run --format=ndjson` (zählt offene Migrationen und Schema-Befehle; der Exit-Code ist dabei immer 0, deshalb wird die Ausgabe gelesen)
und `dbal:run-sql` auf `tl_cron_job` (letzter Lauf eines beliebigen Jobs, der Job `updateMinutelyCliCron` läuft nur über den echten Cron).
Die Matrix „Parameter" zeigt Cron und Migrationen; Warnung, wenn der letzte Cron-Lauf länger als `cron_warn_minutes` (Standard 120) zurückliegt, kritisch ab `cron_crit_minutes` (Standard 240); offene Migrationen sind immer kritisch.
Beide Aufrufe starten den Kernel (einige Sekunden) und laufen bei jedem Zyklus; ein eigenes Intervall je Check ist Punkt 8. Das Alter wird in der Datenbank
mit `NOW()` berechnet und setzt gleiche Zeitzonen in PHP und Datenbank voraus (sonst Hinweis in „Nicht ermittelt"). Geprüft an Contao 5.7, nicht an älteren Versionen.

## Support-Enden (endoflife.date)

Das Cockpit ruft täglich `https://endoflife.date/api/v1/products/{php,contao,mariadb,nginx}/` ab (feste URL, Timeout, Größenlimit; nur ausgehend
vom Cockpit, die Agenten sind nicht beteiligt) und legt die Daten als `eol.json` im Datenverzeichnis ab. Je Minor-Version gilt `eolFrom` (Ende der
Sicherheitsunterstützung): Warnung, wenn es näher als `eol_warn_days` (Standard 90) liegt oder vorbei ist; ohne Datum (z. B. aktueller nginx) kein Urteil.
Die Zelle in den Matrizen wird entsprechend gefärbt. Fällt der Abruf aus, bleiben die letzten Daten gültig; für PHP gibt es zusätzlich die
eingebaute Tabelle `phpEOL` als Rückfall. Mit `-eol=false` ist der Abruf aus (nur diese Tabelle).
Beachten: Das sind die Daten der Hersteller. Distributionen pflegen ältere nginx-/MariaDB-Versionen oft länger mit Backports; so ein Host wird trotzdem gelb.

## Build und Release

```sh
scripts/build.sh                          # agent + cockpit für linux/amd64, linux/arm64, darwin/arm64 nach bin/ (+ SHA256SUMS)
TARGETS="linux/amd64" scripts/build.sh    # nur ein Ziel
```

Die Version zeigen `cockpit -version` und `agent -version`; beide loggen sie beim Start (bei Problemen als Erstes prüfen, welche Version läuft).
Die Version stammt aus `git describe` und wird per `-ldflags -X main.version=…` eingebaut (`dev` ohne Build-Skript).
CI (`.github/workflows/ci.yml`): gofmt, vet, Tests mit `-race`, Build aller Ziele.
Releases macht **release-please** (`release-please-config.json`, `.release-please-manifest.json`) aus Conventional Commits:
auf `main` entsteht ein Release-PR mit CHANGELOG, nach dem Merge Tag `vX.Y.Z`, GitHub-Release und die Binaries als Anhang.
Vor 1.0 hebt `feat:` die Minor-Version, `fix:` die Patch-Version.
**Update-Reihenfolge:** erst Binaries (Agent, Cockpit) austauschen, dann neue Konfigurationsfelder eintragen. Der Agent lehnt unbekannte Schlüssel ab
und startet mit einer neueren Konfiguration auf einem alten Binary nicht.

## Ein Agent für alle Sites des Servers

Statt eines Prozesses pro Site liest der Agent eine Konfigurationsdatei (`agent -config /etc/cockpit-agent/config.json`,
Vorlage `scripts/systemd/cockpit-agent.config.example.json`, Dienst `scripts/systemd/cockpit-agent-local-config.service`).
Die Datei enthält die Tokens und wird abgelehnt, wenn Gruppe oder Andere sie lesen können (`chmod 600`, Eigentümer root).
Ein Token kann stattdessen als `token_file` in einer eigenen, ebenfalls privaten Datei liegen. Unbekannte Schlüssel und
`http://` (außer Loopback, gilt auch für die Einzel-Flags), relative Pfade, doppelte Tokens und Einzel-Flags zusammen mit `-config` werden abgelehnt.
Eine fehlerhafte Site stoppt die anderen nicht; die Sites laufen nacheinander. `host` ist in der Konfiguration standardmäßig `false`
(anders als beim Flag `-host`). `env` ist vertrauenswürdig (auch `PATH` o. Ä. wirkt), deshalb die strengen Dateirechte.

`kind: "host"` meldet nur Servermesswerte (Speicherplatz des Dateisystems aus `path`, Standard `/`; nginx/MariaDB, falls vorhanden),
ohne PHP und Composer. Er erscheint im Cockpit als eigener Eintrag.

**Updates ausschließen:** `"ignore_updates": ["contao/*:6.*", "vendor/paket"]` je Site. Eine Regel ist `paket-glob` (jedes Update) oder
`paket-glob:versions-glob` (nur Updates auf passende Versionen, z. B. keine neue Hauptversion 6.x). `*` passt nicht über `/` hinweg: alle Pakete heißen `*/*`. Das PHP-Bundle kennt die Regeln nicht. Treffer bleiben in der Detailansicht
sichtbar („ausgeschlossen“), zählen aber nicht als offenes Update und lösen keine Warnung aus. Nur in der Konfigurationsdatei, nicht als Flag.

**Host-Werte bei einer Site:** `"host": true` erfasst den freien Speicher (Dateisystem von `path`) und sucht nginx/MariaDB auf dem Server.
Laufen nginx/MariaDB in Docker (je Site eigene Container), findet der Agent dort keine Binaries und meldet bewusst nichts (`–`). Dafür einmal global
`"nginx": "/opt/cockpit/nginx-in-docker", "mariadb": "/opt/cockpit/mariadb-in-docker"` setzen (Wrapper in `scripts/docker/`, wie `composer`/`php`;
sie brauchen `SITE_DIR` in `env` und suchen den Compose-Service `nginx` bzw. `db`; für `kind: "host"` werden sie nicht verwendet). Sie gelten für jede
Site, `"host"` ist dafür nicht nötig. Schlägt ein konfiguriertes Programm fehl, steht das unter „Nicht ermittelt“.
Hat eine Site keine DB oder keinen nginx, setzt man dort je Site `"mariadb": false` bzw. `"nginx": false`: Der Agent sucht nicht danach und meldet kein Problem (in der Matrix steht `–`). `true` ist nicht erlaubt.

`cockpit token <id>` verweigert eine bereits registrierte Site, weil ein neues Token das des laufenden Agenten ungültig macht; ersetzen mit `-rotate`, löschen mit `cockpit token -remove <id>` (nötig, bevor man die Site im Dashboard einem Agenten zuweist).

## Konfiguration aus dem Cockpit

Statt auf jedem Server eine vollständige Datei zu pflegen, bekommt der Agent nur eine kleine lokale Datei; Sites, Intervall und Regeln pflegst du im Dashboard unter **Agenten** (ein Agent je Server, ein Token je Agent).

1. Im Dashboard „Agenten" → Agent anlegen. Das Token erscheint **einmalig** zusammen mit dem Block für die lokale Datei (`bootstrap.json`, chmod 600): `cockpit`, `agent`, `token` (oder `token_file`), `public_key`, `state` (Verzeichnis für Zwischenspeicher und Spool), `roots`, optional `composer`, `php`, `nginx`, `mariadb`.
2. Dort die Sites eintragen (Art, URL, Pfade, Backup, `ignore_updates`, „hat keinen nginx/keine Datenbank", `SITE_DIR`) und speichern.  Unter dem Formular zeigt „Konfiguration, die der Agent erhält" das JSON des gespeicherten Stands.
3. Auf dem Server: `cockpit-agent -bootstrap bootstrap.json` (Vorlagen: `scripts/systemd/cockpit-agent.bootstrap.example.json`, Dienst `scripts/systemd/cockpit-agent.service`). Der Agent holt vor jedem Lauf die Konfiguration (`GET /api/v1/agent/config`, Bearer-Token) und sammelt danach. Den öffentlichen Schlüssel zeigt auch `cockpit pubkey -data ./data`.

Sicherheit:

- Die Konfiguration ist mit **Ed25519** signiert (privater Schlüssel `<data>/signing.key`, chmod 600, wird beim ersten Start angelegt). Der Agent kennt nur den öffentlichen Schlüssel aus seiner lokalen Datei, nicht aus der Antwort. Er prüft Signatur, Empfänger (`agent`), Ablauf (30 Tage) und dass sie nicht älter ist als die zuletzt akzeptierte (kein Zurückspielen).
- Programme (`composer`, `php`, `nginx`, `mariadb`) und das Token bleiben **lokal**. Aus dem Cockpit kommen nur Daten: Pfade müssen unter den lokalen `roots` liegen, als Umgebung ist nur `SITE_DIR` erlaubt. Der Agent prüft das selbst noch einmal.
- Ist das Cockpit nicht erreichbar oder die Antwort ungültig, nimmt der Agent die zuletzt geprüfte, zwischengespeicherte Konfiguration (`<state>/config.json`) bis zu ihrem Ablauf; danach sammelt er nichts mehr, bis wieder eine gültige da ist.
- Ein Agent-Token darf nur für die Sites in der Konfiguration des Agenten melden; eine Site gehört zu genau einem Agenten. Die bisherigen Tokens je Site (`cockpit token`) und `-config` funktionieren unverändert.
- Schreibende Seiten (nur Anlegen, Speichern, Token erneuern, Löschen von Agenten) prüfen `Origin`/`Sec-Fetch-Site`; das Session-Cookie ist `SameSite=Lax`. Das Token wird nie angezeigt außer direkt nach Anlegen/Erneuern.
- Wer den Signaturschlüssel besitzt, bestimmt die Sites und Pfade (innerhalb `roots`) der Agenten, aber keine Programme. Schlüssel und `agents.json` gehören in die Datensicherung; `cockpit pubkey` liefert den Schlüssel für neue Agenten.

Grenzen: `-public-url` setzt die Adresse im angezeigten Block (sonst `https://<Host der Anfrage>`). Die höchste akzeptierte Ausstellungszeit steht in `<state>/last_issued`; wer sie löscht, öffnet ein Fenster für eine ältere, noch gültige Konfiguration. `roots` löst keine Symlinks auf (ein Link innerhalb von `roots` kann nach außen zeigen; nur lokal ausnutzbar). Die `url` einer Site wird nur auf http/https geprüft: wer den Schlüssel hat, kann den Agenten TLS-Verbindungen zu beliebigen Zielen aufbauen lassen. Zeitpunkt der letzten Abholung wird nur im Speicher gehalten (nach einem Neustart des Cockpits leer). Alle Sites eines Agenten teilen sich dessen Token.

## Aufträge (Wartungsmodus)

Das Cockpit kann einem Agenten eine Aktion aus einer **festen Liste** auftragen (`internal/jobs`). Es gibt keine freie Shell; der Auftrag nennt nur den Namen der Aktion, Programm und Argumente stehen im Code. Die meisten Aktionen rufen das Werkzeug des Website-Stacks auf (`<SITE_DIR>/bin/site <befehl>` im Site-Verzeichnis, `SITE_DIR` aus der Site-Konfiguration; `NO_COLOR=1`, ANSI-Steuerzeichen werden aus der Ausgabe entfernt), der Wartungsmodus die Contao-Konsole über den `php`-Wrapper.

| Stufe im Dashboard | Aktion (Name) | Befehl | Arten |
|---|---|---|---|
| Anzeigen (ein Klick) | `site-status`, `site-doctor`, `site-validate-config` | `site status`, `doctor`, `validate-config` | contao, php |
| | `contao-snapshot-list` | `site contao:snapshot:list` | contao |
| | `db-list` | `site db:list` | contao, php |
| Ändern (Bestätigungsseite) | `contao-cache-refresh`, `contao-filesync`, `contao-snapshot` | `site contao:cache:refresh`, `contao:filesync`, `contao:snapshot` | contao |
| | `db-snapshot`, `site-restart` | `site db:snapshot`, `restart` | contao, php |
| | `maintenance-off` | `contao:maintenance-mode disable` | contao |
| Site-ID abtippen | `maintenance-on` | `contao:maintenance-mode enable` | contao |
| | `contao-update` (Zeitlimit 25 Min.) | `site contao:update` | contao |

Alle anderen haben 10 Minuten Zeitlimit. Bewusst nicht dabei: alles, was einen Snapshot-Namen oder eine Rückfrage braucht (Restore, Rollback, Delete; `read -p` bekäme vom Agenten kein stdin), `create`, `delete`, `su`, `container:shell`, `logs:*`, `render`, `pangolin`, `fix-permissions`, `start`/`stop`.

1. **Einschalten auf dem Server:** in der lokalen `bootstrap.json` `"actions": [...]` mit den Namen aus der Tabelle (Beispiel mit allen: `scripts/systemd/cockpit-agent.bootstrap.example.json`). Ohne diese Liste fragt der Agent nie nach Aufträgen. Die Liste ist zugleich die Erlaubnis: Ein kompromittiertes Cockpit kann nie mehr auslösen, als hier steht. Welche davon für welche Site gelten, wählst du auf der Agentenseite je Site (neue Sites haben zunächst keine Freigabe; nach dem Update einmal pro Site ankreuzen, erst Agenten, dann Cockpit aktualisieren).
2. **Freigabe im Dashboard** („Aufträge“, nur mit Login; mit `-no-login` gibt es die Seiten nicht): Site und Aktion wählen, auf der Bestätigungsseite freigeben. Die Stufe steht in der Tabelle; bei „Site-ID abtippen“ muss die ID genau eingetragen werden. Angeboten wird nur, was der Agent beim Abholen als lokal erlaubt meldet, was zur Art der Site passt und was auf der Agentenseite für die Site angekreuzt ist (Whitelist je Site, `actions` in der signierten Konfiguration; leer = nichts; bei „hat keine Datenbank“ entfallen die `db-*`-Aktionen). Der Agent prüft beides nochmals vor dem Ausführen. Jeder angemeldete Benutzer darf freigeben; gespeichert wird, wer es war. Pro Site läuft höchstens ein Auftrag.
3. **Signiert und kurzlebig:** Das Cockpit signiert den Auftrag bei der Freigabe (Ed25519, eigenes Präfix, getrennt von der Konfiguration) mit Ablauf nach 10 Minuten, Empfänger-Agent, Site und eindeutiger ID. Der Agent prüft Signatur, Empfänger, Ablauf, ob die ID neu ist (`<state>/jobs_seen`), ob die Aktion lokal erlaubt ist und ob die Site zu seiner aktuellen Konfiguration gehört und Contao ist.
4. **Eigener Kanal, unabhängig vom Berichtsintervall:** Der Agent fragt per **Long-Polling** (`GET /api/v1/agent/jobs?wait=20`): Das Cockpit hält die Anfrage bis zu 20 Sekunden offen und antwortet sofort, wenn ein Auftrag freigegeben wird. Das sind nur ausgehende Verbindungen, im Leerlauf ca. 3 kleine Anfragen pro Minute, egal ob die Berichte alle 5 oder alle 30 Minuten kommen.
5. **Live-Ausgabe:** Der Agent schickt stdout und stderr alle 0,5 s in nummerierten Blöcken an `POST /api/v1/agent/jobs/{id}/events` (Wiederholung ist harmlos). Die Auftragsseite zeigt sie per **Server-Sent Events** live (nur diese Seite führt ein kleines Skript aus, mit Nonce in der CSP; ohne JavaScript lädt sie sich alle 3 Sekunden neu). Gespeichert werden höchstens 256 KB je Auftrag (danach „gekürzt“), aufbewahrt so lange wie die Tagesstände (`ReportDays`).
6. **Abbrechen:** Wartend wird der Auftrag sofort beendet und der Agent führt ihn nicht mehr aus (er fragt vor dem Start beim Cockpit nach, `running`). Laufend erfährt der Agent es mit der nächsten Meldung (spätestens nach 2 s) und beendet den Prozess. Zeitlimit: je Aktion (siehe Tabelle). „Abgebrochen“ heißt nicht, dass der Befehl nicht gelaufen ist: Zwischen Start und der nächsten Meldung (bis 2 s) kann er schon gewirkt haben; der Status sagt, dass er beendet wurde, nicht dass nichts geschah.
7. Danach meldet der Agent die Site sofort neu, damit z. B. der Wartungsmodus in der Übersicht nicht bis zum nächsten Zyklus fehlt.

Grenzen: `<SITE_DIR>/bin/site` läuft als der Agent-Benutzer (meist root). Der Agent verlangt, dass `SITE_DIR` den Pfad der Site enthält und `bin/site` (nach Auflösen von Verknüpfungen) eine normale Datei unter `roots` ist. Die Dateirechte prüft er nicht: Das Skript darf nicht vom Web-Benutzer beschreibbar sein, sonst führt der Agent dessen Code als root aus. Zwischen der Prüfung und dem Start ist ein Austausch von `bin/site` durch jemanden mit Schreibrecht möglich (Prüfung am aufgelösten, Start am unaufgelösten Pfad). Der Prozess wird bei Abbruch/Zeitlimit direkt beendet; bei den Docker-Wrappern ist das der Wrapper, nicht zwingend der Befehl im Container. Die Ausgabe kann Pfade oder Geheimnisse enthalten und wird nicht gefiltert; sie ist nur für angemeldete Benutzer sichtbar. Ist das Cockpit beim Ende nicht erreichbar, zeigt die Übersicht den Auftrag nach 45 Minuten als „Agent meldet sich nicht mehr“ (dann ist die Site wieder frei, und „Abbrechen“ beendet ihn); ein Auftrag ohne Ausgabe wird in den ersten 2 Minuten nach „läuft“ noch einmal ausgeliefert (verlorene Antwort), der Agent lehnt ihn ab, wenn er ihn schon gesehen hat. Hinter nginx muss für `/jobs/*/stream` das Puffern aus sein (das Cockpit sendet `X-Accel-Buffering: no`) und `proxy_read_timeout` über 30 s liegen. Mehrere Cockpit-Instanzen werden nicht unterstützt (das Wecken der wartenden Anfragen geschieht im Speicher). Der Wartungsmodus wird für das Webroot der Site geschaltet; Varianten je Domain (liegt in der Datenbank) gibt es nicht.

## Sites in Docker-Containern

Der Agent läuft auf dem **Host**, nicht im Container: Das Webroot ist als Bind-Mount lesbar
(`sites/<name>/webroot`, dort `composer.lock` und `var/logs`), ebenso ein Backup-Verzeichnis für `backup_dir`.
Nur `composer` und `php` müssen im Container laufen; dafür gibt es die Wrapper `scripts/docker/composer-in-docker` und
`php-in-docker` (finden den php-Container über die Compose-Labels). Sie brauchen `SITE_DIR`, das pro Site im Feld `env`
der Konfiguration steht; das Arbeitsverzeichnis (`path`) wird in den Container abgebildet.

## Aufbau

| Pfad | Inhalt |
|---|---|
| `internal/report` | Report-Schema (Version 1) und Validierung, von Agent und Cockpit gemeinsam genutzt |
| `internal/collect` | Datensammlung: `composer.lock`, `composer outdated/audit`, Monolog-Fehler, PHP/nginx/MariaDB, Speicherplatz |
| `internal/status` | Bewertung OK/Warnung/Kritisch inkl. PHP-Support-Ende-Tabelle |
| `internal/store` | Speicher-Interface plus Datei-Implementierung (JSON, Verlauf als JSONL) |
| `internal/auth` | Token pro Site, nur SHA-256-Hashes gespeichert |
| `internal/web` | Empfang (`POST /api/v1/reports`), Übersicht, Site-Detail |
| `cmd/agent`, `cmd/cockpit` | die zwei Binaries |

## Lizenz

Das Projekt steht unter der GNU Affero General Public License, Version 3 (AGPL-3.0, siehe `LICENSE`). Wer das Cockpit verändert und als Dienst im Netz anbietet, muss den geänderten Quelltext den Nutzern dieses Dienstes zugänglich machen. Schwachstellen bitte wie in `SECURITY.md` melden.

## Bewusste Lücken (nächste Schritte)

1. **SQLite** ist umgesetzt (siehe „Speicher und Aufbewahrung"). Offen: zusätzlich Auswahl eines Tagesstands in der Oberfläche, Export.
2. **Aufträge per Polling** (Wartungsmodus, Update-Schritte): Agent holt signierte Aufträge ab. Signaturschlüssel, Prüfung, Ablauf und Host-Token gibt es seit der Fernkonfiguration; die feste Aktionsliste (Wartungsmodus an/aus), die Freigabe im Dashboard, Long-Polling und die Live-Ausgabe gibt es seit „Aufträge“. Offen: weitere Aktionen (Updates). Siehe Doc „Kommunikation".
3. **Wartungsmodus** wird bei `kind: "contao"` ausgelesen: Der Agent prüft, ob `var/maintenance.html` existiert (die Datei, die `contao:maintenance-mode enable` anlegt, manager-bundle; Contao 4.13+). Er zeigt „an seit …/aus" in Detailseite und Übersicht („Wartung"). Der Wartungsmodus an sich ist kein Fehler; ist er länger als `maintenance_warn_minutes` (Standard 120) an, gibt es eine Warnung, ab `maintenance_crit_minutes` (Standard 480) kritisch („vergessen"). Der Zeitpunkt ist das Änderungsdatum der Datei. Offen: Wartungsmodus je Website-Root (liegt in der Datenbank), abweichender Dateipfad.
4. **Weitere Checks:** TLS-Ablauf und Backup-Alter sind umgesetzt (Agent-Flag `-backup-dir`, Schwellwerte in `status.Config`). Support-Ende von PHP, Contao, MariaDB und nginx ist umgesetzt (endoflife.date, siehe „Support-Enden"). Ebenso umgesetzt: offene Migrationen und letzter Contao-Cron-Lauf (siehe „Contao-Konsole"). Offen: Wartungsmodus.
5. **Alarmierung** per E-Mail (SMTP) ab „kritisch" ist umgesetzt (`internal/alert`, Flags `-alert-to/-alert-from/-alert-smtp`, Zugang über `COCKPIT_SMTP_USER/_PASSWORD`; Alarm beim Wechsel nach kritisch, Entwarnung beim Wechsel zurück). Offen: Zustand überlebt keinen Neustart, keine Erinnerung bei langem Alarm, kein Port 465, kein ntfy.
6. **Absicherung des Cockpits:** Login, Rate-Limits, Request-Signatur (HMAC) zusätzlich zum Bearer-Token.
   Entscheidung Login: **OIDC im Cockpit gegen Authentik** (Passkeys macht Authentik). Das Cockpit verlässt sich nicht auf den Proxy (Pangolin), auch wenn dort die Ressource ungeschützt ist; nur `/api/` und `/healthz` sind per Pfadregel vom SSO ausgenommen. Mehrere Benutzer möglich (Zugriff über `sub`/E-Mail-Liste oder Authentik-Gruppe).
   - [x] a) Routen getrennt: UI hinter `Server.Protect`, Agent-API (Bearer) und `/healthz` davor.
   - [x] b) OIDC-Flow (`internal/login`; Authorization Code + PKCE, `coreos/go-oidc` + `x/oauth2`; `go` in go.mod deshalb auf 1.26), Provider-Stub im Test.
   - [x] c) Session (`login.Sessions`) als signiertes Cookie (`HttpOnly`, `Secure`, `SameSite=Lax`), Ablaufzeit, Schlüssel aus der Konfiguration.
   - [x] d) Zugriffsbeschränkung (`login.Access`: Benutzer, verifizierte E-Mail, Gruppe) und Verdrahtung in `cmd/cockpit`; `serve` bricht ohne Login-Konfiguration ab, außer mit `-no-login` (die Demo nutzt es).
   - [x] e) Rate-Limit pro IP (`internal/ratelimit`) auf `/login`, `/callback` (10/min) und die Agent-API (120/min, Burst für den Spool). Hinter Pangolin `-trusted-proxy <IP/CIDR des Proxys>` setzen, sonst teilen sich alle Clients die Proxy-IP und damit ein Limit; `X-Forwarded-For` wird nur von dort ausgewertet.
7. **PHP-Bundle / Cron-Skript** für Shared Hosting, gleiches JSON-Schema.
8. **Abfrageintervall der Checks konfigurierbar machen.** Aktuell wird das TLS-Zertifikat (und alles andere) bei jedem Zyklus (Standard 5 Min.) neu geprüft; später per Konfiguration einstellbar, wie oft welcher Check läuft.
9. **Logout-Button** ist umgesetzt (nur mit Login sichtbar; CSP `form-action 'self'`). Offen: Das Sitzungs-Cookie wird nur im Browser gelöscht, es gibt keine serverseitige Sperrliste, und der Identity-Provider (Authentik) bleibt angemeldet; ein echtes Single Logout bräuchte RP-initiated Logout.
10. **Ausschlussregeln** (`ignore_updates`) sind nur in der Agent-Konfiguration pflegbar; später im Cockpit mit Freigabe, ebenso Anzeige, welche Regel gegriffen hat.
11. **Betrieb:** Die Wrapper `scripts/docker/*-in-docker` sind für nginx/MariaDB/php/composer bei den eigenen Sites erprobt, nicht allgemein getestet.
12. **Matrix-Übersicht** ist umgesetzt: zwei Matrizen unter der Site-Liste (Versionen: Contao, PHP, MariaDB, Nginx; Parameter: Platz frei, letztes Backup, TLS), alle Sites/Hosts, fehlende Werte als „–". Gefärbt wird nur, wo es Schwellen gibt (PHP-Support-Ende, Backup-Alter, TLS-Ablauf, freier Speicher). Offen: Filter, weitere Spalten, Schwellen für Contao-, MariaDB- und Nginx-Versionen.
14. **Konfiguration vom Cockpit abholen** ist umgesetzt (siehe „Konfiguration aus dem Cockpit"). Offen: Agent-Zustand (Version, letzter Lauf) im Dashboard, Änderungsprotokoll der Konfiguration.
15. **Trends** sind auf der Detailseite umgesetzt: wählbarer Zeitraum (`?range=1|7|30|90|365`, 1 = letzte 24 Stunden; der Zeitraum beginnt immer bei „jetzt minus Dauer", fehlende frühere Werte bleiben leer), Statusband und serverseitig gerenderte SVG-Kurven (ohne JavaScript) für freien Speicher, offene Updates, Fehler sowie Cron- und Backup-Alter (je Pixel-Slot der schlimmste Wert), dazu die Liste der Versionswechsel. In der Übersicht zeigen kleine Pfeile die Veränderung von freiem Speicher (ab 2 Prozentpunkten) und offenen Updates (ab 1) gegenüber dem ältesten Messpunkt der letzten 7 Tage; erst ab 24 Stunden Historie, sonst kein Pfeil. Das Statusband zeigt Zeiten ohne Meldung (länger als `stale_after_minutes`) grau; das gilt nur für die unverdichteten Punkte (`history_full_days`). Auf der Detailseite lässt sich über die Auswahl „Historische Werte anzeigen“ oben rechts (`?day=JJJJ-MM-TT`, UTC-Tag; Anzeige TT.MM.JJJJ, neueste zuerst) der letzte Report eines Tages ansehen, bewertet wie zum damaligen Zeitpunkt. Bei fallendem freiem Speicher steht neben dem Pfeil „voll in ~N Tg." (gerade Linie vom Vergleichspunkt bis jetzt, nur bis 90 Tage; grobe Schätzung, Aufräumen und Schwankungen werden nicht berücksichtigt). Die Übersicht lässt sich nach Name/ID/URL, Status (Warnung und Kritisch, nur Kritisch) und Art filtern (GET-Formular, `?q=&status=&kind=`; gilt für alle drei Tabellen, die Kacheln zählen weiter alle Sites; Sites ohne Meldung haben noch keine Art und fallen bei einem Art-Filter heraus).
13. Mockups (Übersicht, Detail, Update-Freigabe) liegen im Canvas „Contao Cockpit – Mockups"; Update-Freigabe ist noch nicht umgesetzt.
