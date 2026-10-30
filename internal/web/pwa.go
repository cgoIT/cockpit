package web

import (
	"embed"
	"net/http"
	"runtime/debug"
)

// versionText: Version und, falls der Build aus einem Git-Checkout stammt, die ersten 8 Stellen des Commits (Fehlersuche).
func (s *Server) versionText() string {
	v := s.Version
	if v == "" {
		v = "dev"
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range bi.Settings {
			if kv.Key == "vcs.revision" && len(kv.Value) >= 8 {
				return v + " (" + kv.Value[:8] + ")"
			}
		}
	}
	return v
}

// PNG-Symbole (erzeugt mit scripts/genicons/main.go); iOS und ältere Browser brauchen PNG.
//
//go:embed static/*.png
var staticFS embed.FS

// shortcut: ohne Service Worker (kein Offlinebetrieb), kein "maskable"-Symbol; bei Bedarf ergänzen.
const manifestJSON = `{
  "name": "Sitewatch",
  "short_name": "Sitewatch",
  "description": "Überwachung und Wartung von Contao- und PHP-Webseiten",
  "lang": "de",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#F4F5F7",
  "theme_color": "#1F5FBF",
  "icons": [
    {"src": "/icon-192.png", "sizes": "192x192", "type": "image/png"},
    {"src": "/icon-512.png", "sizes": "512x512", "type": "image/png"},
    {"src": "/icon.svg", "sizes": "any", "type": "image/svg+xml"}
  ]
}`

const iconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512"><rect width="512" height="512" rx="96" fill="#1F5FBF"/><path d="M64 272h96l48-112 80 208 56-128h104" fill="none" stroke="#fff" stroke-width="40" stroke-linecap="round" stroke-linejoin="round"/></svg>`

func manifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(manifestJSON))
}

func icon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(iconSVG))
}

func png(name string) http.HandlerFunc {
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
	}
}
