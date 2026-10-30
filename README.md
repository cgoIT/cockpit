# Sitewatch

Überwachung und Wartung von Contao- und PHP-Webseiten auf eigenen Servern.

Auf jedem Server läuft ein kleiner **Agent**. Er sammelt Zustandsdaten und schickt sie **ausgehend per HTTPS** an das
**Dashboard**. Auf den Zielservern wird kein Port geöffnet.

Was das Dashboard zeigt (je Site und für den Server):

- Versionen von Contao, PHP, MariaDB und nginx, dazu das Ende der Herstellerunterstützung (endoflife.date)
- offene Composer-Updates und Sicherheitshinweise (`composer outdated/audit`), mit Ausschlussregeln
- letzte Fehler aus den Monolog-Logs, offene Contao-Migrationen, letzter Contao-Cron-Lauf
- freier Speicherplatz, Alter des letzten Backups, Ablauf des TLS-Zertifikats, Wartungsmodus
- Verlauf und Trends (Kurven, Statusband, Tagesstände) und eine Bewertung OK / Warnung / Kritisch
- Alarmierung per E-Mail, wenn eine Site kritisch wird
- Aufträge aus einer **festen Liste** (z. B. Wartungsmodus, Cache leeren, Snapshot), jeweils mit Freigabe im Dashboard und Live-Ausgabe

Sitewatch ist ein Go-Programm mit serverseitig gerenderter Oberfläche und SQLite als Speicher. Es gibt zwei Binaries:
`sitewatch` (Dashboard) und `sitewatch-agent`.

## Installation (Debian/Ubuntu, amd64 und arm64)

```sh
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL https://cgoit.github.io/sitewatch/key.asc -o /etc/apt/keyrings/sitewatch.asc
echo "deb [signed-by=/etc/apt/keyrings/sitewatch.asc] https://cgoit.github.io/sitewatch stable main" | sudo tee /etc/apt/sources.list.d/sitewatch.list
sudo apt update
sudo apt install sitewatch-agent    # auf den Servern mit Sites
sudo apt install sitewatch          # auf dem Server des Dashboards
```

Die Pakete legen Binary und systemd-Unit an (beim Dashboard auch den Benutzer `sitewatch`), **starten aber nichts**.
Updates kommen mit `apt upgrade`; die Konfiguration bleibt unangetastet. Die `.deb`-Dateien hängen außerdem an jedem
GitHub-Release.

**Reihenfolge bei Updates:** erst die Binaries austauschen (Dashboard und Agenten), dann neue Konfigurationsfelder
eintragen. Der Agent lehnt unbekannte Schlüssel ab. Dashboard und Agenten sollten dieselbe Version haben.

Ohne apt: `scripts/build.sh` (siehe „Selbst bauen") und die Binaries samt Units aus `scripts/systemd/` von Hand installieren.

## Konfiguration

### 1. Dashboard

`/etc/sitewatch/sitewatch.env` ausfüllen (die Vorlage liegt schon dort):

```sh
SITEWATCH_ARGS=-public-url https://sitewatch.example -allow-group sitewatch -trusted-proxy 127.0.0.1
SITEWATCH_OIDC_ISSUER=https://auth.example/application/o/sitewatch/
SITEWATCH_OIDC_CLIENT_ID=...
SITEWATCH_OIDC_CLIENT_SECRET=...
SITEWATCH_OIDC_REDIRECT_URL=https://sitewatch.example/callback
SITEWATCH_SESSION_KEY=...        # openssl rand -hex 32; fest halten, sonst verfallen alle Sitzungen
```

Dann `systemctl enable --now sitewatch`. Der Dienst lauscht auf `127.0.0.1:8080`; davor gehört ein Reverse-Proxy mit TLS
(bei nginx für `/jobs/*/stream` das Puffern ausschalten und `proxy_read_timeout` über 30 s setzen).

- **Login:** OIDC (getestet mit Authentik). Zugriff über `-allow-group`, `-allow-email` oder `-allow-sub`. Ohne Login-Konfiguration
  startet `serve` nicht; nur zum Entwickeln gibt es `-no-login`. Nur `/api/` (Agent-Token) und `/healthz` sind ohne Login erreichbar.
  `-trusted-proxy` ist die IP/das Netz des Proxys, sonst teilen sich alle Clients dessen IP und damit ein Rate-Limit.
- **Daten:** `/var/lib/sitewatch` (`sitewatch.db`, `signing.key`, `agents.json`, `tokens.json`). Gehört in die Sicherung;
  ohne `signing.key` müssten alle Agenten den neuen öffentlichen Schlüssel erhalten. Die Datenbank sichert man mit
  `sqlite3 sitewatch.db ".backup ziel.db"`.
- **Alarm per E-Mail:** `-alert-to`, `-alert-from`, `-alert-smtp`; Zugang über `SITEWATCH_SMTP_USER` und `SITEWATCH_SMTP_PASSWORD`.
- **Schwellwerte** (Standardwerte, alle optional): `sitewatch serve -thresholds schwellen.json`. Fehlende Felder behalten den
  Standard, unbekannte Felder oder Werte unter 1 lassen den Start scheitern.

  ```json
  {
  "stale_after_minutes": 15,
  "error_warn": 10,
  "eol_warn_days": 90,
  "tls_warn_days": 14,
  "backup_max_hours": 36,
  "disk_warn_pct": 15,
  "disk_crit_pct": 5,
  "cron_warn_minutes": 120,
  "cron_crit_minutes": 240,
  "maintenance_warn_minutes": 120,
  "maintenance_crit_minutes": 480,
  "history_full_days": 30,
  "history_6h_days": 90,
  "history_max_days": 365,
  "history_reports_days": 90
  }
  ```

  `disk_*_pct` ist der freie Speicher in Prozent. Der Verlauf wird mit der Zeit verdichtet (30 Tage jeder Punkt, bis 90 Tage
  alle 6 Stunden, bis 365 Tage einer je Tag).
- **Support-Enden:** Das Dashboard ruft täglich endoflife.date ab (`-eol=false` schaltet das aus).

### 2. Agent anlegen

Im Dashboard unter **Agenten** einen Agenten anlegen (einer je Server). Das Token erscheint **einmalig**, zusammen mit dem
Block für die lokale Datei. Dort legst du auch die Sites fest (Art, URL, Pfade, Backup-Verzeichnis, `ignore_updates`,
„hat keine Datenbank/keinen nginx", `SITE_DIR`).

Auf dem Server `/etc/sitewatch-agent/bootstrap.json` anlegen (`chmod 600`; Vorlagen unter
`/usr/share/doc/sitewatch-agent/examples/`):

```json
{
  "sitewatch": "https://sitewatch.example",
  "agent": "server1",
  "token": "TOKEN-DES-AGENTEN",
  "public_key": "AUSGABE-VON-sitewatch-pubkey",
  "state": "/var/lib/sitewatch-agent",
  "roots": ["/appl"],
  "composer": "/usr/share/sitewatch-agent/docker/composer-in-docker",
  "php": "/usr/share/sitewatch-agent/docker/php-in-docker"
}
```

Dann `systemctl enable --now sitewatch-agent`. Der Agent holt vor jedem Lauf die Konfiguration (Sites, Intervall) signiert vom
Dashboard; den öffentlichen Schlüssel zeigt `sitewatch pubkey -data /var/lib/sitewatch`. Programme (`composer`, `php`, `nginx`,
`mariadb`), Token und `roots` bleiben lokal, aus dem Dashboard kommen nur Daten. Fällt das Dashboard aus, arbeitet der Agent
mit der zuletzt geprüften Konfiguration weiter, bis sie abläuft.

Für Server, die ohne Dashboard-Konfiguration laufen sollen, gibt es `sitewatch-agent -config datei.json`
(Vorlage und Unit unter `/usr/share/doc/sitewatch-agent/examples/`).

**Art einer Site (`kind`):** `contao` (Contao-Konsole, Migrationen, Cron), `php` (sonstige PHP-Site) und `host` (nur
Servermesswerte: Speicherplatz, nginx, MariaDB; erscheint als eigener Eintrag).

**Updates ausschließen:** `"ignore_updates": ["contao/*:6.*", "vendor/paket"]` je Site. Eine Regel ist `paket-glob` (jedes Update)
oder `paket-glob:versions-glob`. Treffer bleiben sichtbar („ausgeschlossen"), zählen aber nicht als offenes Update.

### 3. Aufträge (optional)

Ohne Freigabe auf dem Server fragt der Agent nie nach Aufträgen. Zum Einschalten in der `bootstrap.json` eine Liste
`"actions": [...]` eintragen (Beispiel mit allen Namen: `sitewatch-agent.bootstrap.example.json` unter `/usr/share/doc/sitewatch-agent/examples/`). Sie ist die lokale Obergrenze:
Ein kompromittiertes Dashboard kann nie mehr auslösen. Auf der Agentenseite wählst du dann je Site, welche davon gelten
(Standard: keine). Ausgelöst wird im Dashboard unter **Aufträge** mit Freigabe; Aufträge sind signiert und 10 Minuten gültig.

| Stufe | Aktionen |
|---|---|
| Anzeigen (ein Klick) | `site-status`, `site-doctor`, `site-validate-config`, `contao-snapshot-list`, `db-list` |
| Ändern (Bestätigungsseite) | `contao-cache-refresh`, `contao-filesync`, `contao-snapshot`, `db-snapshot`, `site-restart`, `maintenance-off` |
| Site-ID abtippen | `maintenance-on`, `contao-update` |

Die meisten Aktionen rufen `<SITE_DIR>/bin/site <befehl>` auf (das Werkzeug des Website-Stacks); der Wartungsmodus nutzt die
Contao-Konsole. Es gibt keine freie Shell. `bin/site` läuft als Benutzer des Agenten (meist root) und darf deshalb nicht vom
Web-Benutzer beschreibbar sein.

## Sites in Docker-Containern

Der Agent läuft auf dem **Host**, nicht im Container. Das Webroot ist dort per Bind-Mount lesbar (`composer.lock`, `var/logs`),
ebenso das Backup-Verzeichnis. Nur `composer`, `php` und bei Bedarf `nginx` und `mariadb` müssen im Container laufen. Dafür gibt
es Wrapper-Skripte in [`scripts/docker/`](scripts/docker):

| Skript | ersetzt | Compose-Service |
|---|---|---|
| `composer-in-docker`, `php-in-docker` | `composer`, `php` | `php` |
| `nginx-in-docker` | `nginx` | `nginx` |
| `mariadb-in-docker` | `mariadb` | `db` |

Das Paket `sitewatch-agent` installiert sie nach `/usr/share/sitewatch-agent/docker/`; in der `bootstrap.json` eintragen (beim Anlegen oder Erneuern eines Agenten im Dashboard kreuzt man an, welche Programme in Docker laufen; die anderen werden direkt auf dem Host aufgerufen)
(`composer`, `php`, `nginx`, `mariadb`). Sie finden den Container über die Compose-Labels und brauchen `SITE_DIR`, den absoluten Pfad
des Site-Verzeichnisses mit der `compose.yml`; er steht je Site in der Agentenkonfiguration (`env`). Das Arbeitsverzeichnis der
Site wird in den Container abgebildet. Die Wrapper sind bei den eigenen Sites erprobt, nicht allgemein getestet.

Hat eine Site keinen nginx oder keine Datenbank, ist das je Site einstellbar (`"nginx": false`, `"mariadb": false`); der Agent
meldet dann kein Problem. Was der Agent nicht ermitteln konnte, steht in der Detailansicht unter „Nicht ermittelt".

## Schnell ausprobieren (Demo)

```sh
./scripts/demo.sh          # baut, startet das Dashboard auf :8099 und lässt Demo-Agenten laufen
# Browser: http://127.0.0.1:8099
kill $(cat /tmp/sitewatch-demo/sitewatch.pid)
```

Die Demo startet ohne Login (`-no-login`) und nutzt Beispieldaten aus `testdata/` sowie einen **composer-Stub**. Nichts davon sind
echte Zahlen.

Gegen eine echte Site, ohne Server-Einrichtung:

```sh
go build -o bin/ ./cmd/...
bin/sitewatch token meine-site > token.txt             # Token einmalig anzeigen, Hash landet in tokens.json
bin/sitewatch serve -addr 127.0.0.1:8080 -no-login     # nur lokal; für den Betrieb OIDC einrichten
bin/agent -site-id meine-site -name "Meine Site" -kind contao -url https://example.org \
  -path /var/www/meine-site -sitewatch http://127.0.0.1:8080 -token "$(cat token.txt)" -once
```

Mit `-dry-run` gibt der Agent den Report nur aus, ohne zu senden. `-spool <dir>` puffert Reports, wenn das Dashboard nicht erreichbar ist.

## Selbst bauen

Voraussetzung ist eine aktuelle Go-Version (siehe `go.mod`).

```sh
go vet ./... && go test ./...
scripts/build.sh                          # Agent und Dashboard für linux/amd64, linux/arm64, darwin/arm64 nach bin/ (+ SHA256SUMS)
TARGETS="linux/amd64" scripts/build.sh    # nur ein Ziel
VERSION=0.1.0 scripts/build-deb.sh        # .deb-Pakete (nutzt nfpm über `go run`)
scripts/build-apt.sh                      # signiertes apt-Repository (braucht apt-ftparchive und einen GPG-Schlüssel)
```

`sitewatch -version` und `sitewatch-agent -version` zeigen die Version (aus `git describe`; `dev` ohne Build-Skript).

## Lizenz

GNU Affero General Public License, Version 3 (AGPL-3.0, siehe [`LICENSE`](LICENSE)). Wer Sitewatch verändert und als Dienst im Netz
anbietet, muss den geänderten Quelltext den Nutzern dieses Dienstes zugänglich machen. Schwachstellen bitte wie in
[`SECURITY.md`](SECURITY.md) melden.
