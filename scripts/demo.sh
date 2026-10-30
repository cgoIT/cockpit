#!/bin/bash
# Startet Sitewatch lokal und lässt Beispiel-Agenten gegen eine Demo-Umgebung laufen.
# Alle Namen, Domains und Zahlen sind Beispieldaten. composer ist durch einen Stub ersetzt.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$(pwd)
DEMO=${DEMO_DIR:-/tmp/sitewatch-demo}
PORT=${PORT:-8099}
rm -rf "$DEMO"; mkdir -p "$DEMO/sites" "$DEMO/bin"
go build -o "$DEMO/bin/" ./cmd/...

ago() { date -u -d "-$1 minutes" +%Y-%m-%dT%H:%M:%S.000000+00:00; }

mksite() { # id contao-version
  local d="$DEMO/sites/$1"; mkdir -p "$d/var/logs" "$d/.demo"
  cat > "$d/composer.lock" <<LOCK
{"packages":[{"name":"contao/core-bundle","version":"$2"},{"name":"contao/manager-bundle","version":"$2"},{"name":"symfony/http-kernel","version":"6.4.9"},{"name":"doctrine/dbal","version":"3.8.4"},{"name":"twig/twig","version":"3.10.3"}]}
LOCK
  mkdir -p "$d/vendor/bin"
  cat > "$d/vendor/bin/contao-console" <<'PHP'
<?php // DEMO-STUB: keine echte Contao-Konsole, liefert feste Beispielausgaben
if (in_array('contao:migrate', $argv)) {
    echo '{"type":"migration-pending","names":[],"hash":"demo"}', "\n", '{"type":"schema-pending","commands":[],"hash":"demo"}', "\n";
} else {
    echo " -----\n  age\n -----\n  120\n -----\n";
}
PHP
}

# eigene-seite-a: alles aktuell
mksite eigene-seite-a v5.7.1

# kunde-b: ein paar Updates, wenige Fehler
mksite kunde-b v5.7.1
cat > "$DEMO/sites/kunde-b/.demo/outdated.json" <<J
{"installed":[
 {"name":"doctrine/dbal","version":"3.8.4","latest":"3.9.3","latest-status":"semver-safe-update"},
 {"name":"twig/twig","version":"3.10.3","latest":"3.14.2","latest-status":"semver-safe-update"},
 {"name":"vendor/beispiel-erweiterung","version":"2.1.0","latest":"3.0.0","latest-status":"update-possible"}]}
J
echo "[$(ago 90)] request.ERROR: Database connection timed out {\"exception\":\"x\"} []" > "$DEMO/sites/kunde-b/var/logs/prod-demo.log"

# kunde-c: Sicherheitshinweis, viele Updates, viele Fehler
mksite kunde-c v5.3.9
cat > "$DEMO/sites/kunde-c/.demo/outdated.json" <<J
{"installed":[
 {"name":"symfony/http-kernel","version":"6.4.9","latest":"6.4.14","latest-status":"semver-safe-update"},
 {"name":"contao/core-bundle","version":"5.3.9","latest":"5.3.14","latest-status":"semver-safe-update"},
 {"name":"contao/manager-bundle","version":"5.3.9","latest":"5.3.14","latest-status":"semver-safe-update"},
 {"name":"doctrine/dbal","version":"3.8.4","latest":"3.9.3","latest-status":"semver-safe-update"},
 {"name":"vendor/beispiel-erweiterung","version":"2.1.0","latest":"3.0.0","latest-status":"update-possible"}]}
J
cat > "$DEMO/sites/kunde-c/.demo/audit.json" <<J
{"advisories":{"symfony/http-kernel":[{"advisoryId":"DEMO-1","packageName":"symfony/http-kernel","title":"Beispiel-Sicherheitshinweis (Demo)","cve":"CVE-0000-0000","link":"https://example.org/advisory"}]}}
J
{ for i in 5 12 25 40 70 95 130 180 240 300 400 500; do
    echo "[$(ago $i)] request.CRITICAL: Uncaught exception in Frontend module {\"exception\":\"x\"} []"; done
  echo "[$(ago 200)] app.ERROR: Cron job failed [] []"
} > "$DEMO/sites/kunde-c/var/logs/prod-demo.log"

# seite-f: reine PHP-Seite mit altem PHP (Stub)
mkdir -p "$DEMO/sites/seite-f"

TOK="$DEMO/tokens.json"
for s in eigene-seite-a kunde-b kunde-c seite-f kunde-h; do
  "$DEMO/bin/sitewatch" token -tokens "$TOK" "$s" > "$DEMO/token-$s" 2>/dev/null
done

# -no-login: nur für die Demo, nie produktiv
"$DEMO/bin/sitewatch" serve -no-login -addr "127.0.0.1:$PORT" -data "$DEMO/data" -tokens "$TOK" > "$DEMO/sitewatch.log" 2>&1 &
echo $! > "$DEMO/sitewatch.pid"
for _ in $(seq 1 50); do curl -fs "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.1; done

run() { # id name kind url [weitere agent-flags]
  local id=$1 name=$2 kind=$3 url=$4; shift 4
  "$DEMO/bin/agent" -once -site-id "$id" -name "$name" -kind "$kind" -url "$url" -path "$DEMO/sites/$id" \
    -sitewatch "http://127.0.0.1:$PORT" -token "$(cat "$DEMO/token-$id")" -composer "$ROOT/testdata/bin/composer" "$@"
}
run eigene-seite-a "Eigene Seite A" contao https://seite-a.example
run kunde-b        "Kunde B"        contao https://kunde-b.example
run kunde-c        "Kunde C"        contao https://kunde-c.example
run seite-f        "Einfache PHP-Seite F" php https://seite-f.example -php "$ROOT/testdata/bin/php-8.1"
# kunde-h ist registriert, hat aber nie gemeldet

echo "Sitewatch: http://127.0.0.1:$PORT   (beenden: kill \$(cat $DEMO/sitewatch.pid))"
