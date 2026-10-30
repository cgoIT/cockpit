#!/usr/bin/env bash
# Baut aus den .deb-Dateien in dist/ ein signiertes apt-Repository nach apt-site/ (zum Veröffentlichen auf GitHub Pages).
# Voraussetzungen: apt-ftparchive (Paket apt-utils) und ein importierter geheimer GPG-Schlüssel (nur zum Signieren, ohne Passphrase).
#   scripts/build-apt.sh                 Schlüssel: der erste geheime im Schlüsselbund
#   KEY=<Fingerabdruck> scripts/build-apt.sh
# shortcut: Das Repository enthält nur die Pakete aus dist/ (die neueste Version); ältere Versionen bleiben als Anhang der GitHub-Releases erreichbar.
set -euo pipefail
cd "$(dirname "$0")/.."

DEBS=${DEBS:-dist}
OUT=${OUT:-apt-site}
BASE_URL=${BASE_URL:-https://cgoit.github.io/sitewatch}
SUITE=stable
ARCHS=${ARCHS:-"amd64 arm64"}
KEY=${KEY:-$(gpg --list-secret-keys --with-colons | awk -F: '/^fpr:/ {print $10; exit}')}
[ -n "$KEY" ] || { echo "kein geheimer GPG-Schlüssel gefunden" >&2; exit 1; }
ls "$DEBS"/*.deb >/dev/null

rm -rf "$OUT" && mkdir -p "$OUT/pool/main"
cp "$DEBS"/*.deb "$OUT/pool/main/"
touch "$OUT/.nojekyll"
cd "$OUT"

for arch in $ARCHS; do
  d="dists/$SUITE/main/binary-$arch"
  mkdir -p "$d"
  apt-ftparchive --arch "$arch" packages pool > "$d/Packages"
  gzip -9kn "$d/Packages"
done
apt-ftparchive \
  -o APT::FTPArchive::Release::Origin=Sitewatch \
  -o APT::FTPArchive::Release::Label=Sitewatch \
  -o APT::FTPArchive::Release::Suite=$SUITE \
  -o APT::FTPArchive::Release::Codename=$SUITE \
  -o "APT::FTPArchive::Release::Architectures=$ARCHS" \
  -o APT::FTPArchive::Release::Components=main \
  release "dists/$SUITE" > "dists/$SUITE/Release"

gpgsign=(gpg --batch --yes --pinentry-mode loopback --local-user "$KEY")
"${gpgsign[@]}" --clearsign -o "dists/$SUITE/InRelease" "dists/$SUITE/Release"
"${gpgsign[@]}" --armor --detach-sign -o "dists/$SUITE/Release.gpg" "dists/$SUITE/Release"
gpg --armor --export "$KEY" > key.asc

cat > index.html <<EOF
<!doctype html>
<html lang="de"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sitewatch – apt-Repository</title></head>
<body style="font-family:system-ui,sans-serif;max-width:46rem;margin:2rem auto;padding:0 1rem">
<h1>Sitewatch – apt-Repository</h1>
<p>Pakete <code>sitewatch</code> und <code>sitewatch-agent</code> für Debian/Ubuntu (amd64, arm64). Quelltext: <a href="https://github.com/cgoIT/sitewatch">github.com/cgoIT/sitewatch</a>.</p>
<pre>sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL $BASE_URL/key.asc -o /etc/apt/keyrings/sitewatch.asc
echo "deb [signed-by=/etc/apt/keyrings/sitewatch.asc] $BASE_URL $SUITE main" | sudo tee /etc/apt/sources.list.d/sitewatch.list
sudo apt update
sudo apt install sitewatch-agent   # oder: sitewatch</pre>
</body></html>
EOF
