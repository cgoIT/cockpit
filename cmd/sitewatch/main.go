// Sitewatch nimmt Reports der Agenten entgegen und zeigt den Zustand aller Sites.
//
//	sitewatch serve [-addr :8080] [-data ./data] [-tokens ./tokens.json] [-allow-group g] [-allow-sub s] [-allow-email e] [-trusted-proxy ip,cidr] [-no-login] [-alert-to a,b -alert-from x -alert-smtp host:port]
//	sitewatch pubkey [-data ./data]
//	sitewatch version
//	sitewatch token [-tokens ./tokens.json] [-kind contao|php|host] [-name n] [-url u] [-path p] [-rotate | -remove] <site-id>
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sitewatch/internal/agents"
	"sitewatch/internal/alert"
	"sitewatch/internal/auth"
	"sitewatch/internal/eol"
	"sitewatch/internal/login"
	"sitewatch/internal/ratelimit"
	"sitewatch/internal/sign"
	"sitewatch/internal/status"
	"sitewatch/internal/store"
	"sitewatch/internal/web"
)

// version wird beim Build per -ldflags "-X main.version=..." gesetzt (scripts/build.sh).
var version = "dev"

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "-version" || args[0] == "--version") {
		args[0] = "version"
	}
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "version":
		fmt.Println("sitewatch", version)
	case "serve":
		serve(args)
	case "token":
		token(args)
	case "pubkey":
		pubkey(args)
	default:
		fmt.Fprintln(os.Stderr, "Verwendung: sitewatch serve | sitewatch token <site-id> | sitewatch pubkey | sitewatch version")
		os.Exit(2)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "Adresse, auf der gelauscht wird (hinter nginx mit TLS betreiben)")
	data := fs.String("data", "./data", "Verzeichnis für gespeicherte Reports")
	tokens := fs.String("tokens", "./tokens.json", "Datei mit Token-Hashes der Agenten")
	stale := fs.Duration("stale", 15*time.Minute, "keine Meldung seit dieser Zeit = kritisch")
	noLogin := fs.Bool("no-login", false, "Oberfläche OHNE Login betreiben (nur lokale Entwicklung)")
	group := fs.String("allow-group", "", "Authentik-Gruppen mit Zugriff (kommagetrennt)")
	sub := fs.String("allow-sub", "", "Benutzer-IDs (OIDC sub) mit Zugriff (kommagetrennt)")
	email := fs.String("allow-email", "", "verifizierte E-Mail-Adressen mit Zugriff (kommagetrennt)")
	proxies := fs.String("trusted-proxy", "", "IPs/CIDRs des Reverse-Proxys, dessen X-Forwarded-For für das Rate-Limit gilt (kommagetrennt)")
	thresholds := fs.String("thresholds", "", "JSON-Datei mit Schwellwerten (siehe README); leer = Standardwerte")
	useEOL := fs.Bool("eol", true, "Support-Enden täglich von endoflife.date abrufen (PHP, Contao, MariaDB, nginx); aus = nur die eingebaute PHP-Tabelle")
	publicURL := fs.String("public-url", "", "Adresse von Sitewatch für den Block der Agenten-Datei (z. B. https://sitewatch.example); leer = https://<Host der Anfrage>")
	signKey := fs.String("signing-key", "", "privater Ed25519-Schlüssel für die Konfiguration der Agenten (Standard: <data>/signing.key, wird angelegt)")
	agentsFile := fs.String("agents", "", "Datei mit Agenten und deren Konfiguration (Standard: <data>/agents.json)")
	alertTo := fs.String("alert-to", "", "Empfänger der Alarm-E-Mails (kommagetrennt); leer = keine Alarmierung. SMTP-Zugang: SITEWATCH_SMTP_USER/_PASSWORD in der Umgebung")
	alertFrom := fs.String("alert-from", "", "Absender der Alarm-E-Mails")
	alertSMTP := fs.String("alert-smtp", "localhost:25", "SMTP-Server host:port (STARTTLS, wenn angeboten)")
	alertEvery := fs.Duration("alert-interval", time.Minute, "Prüfintervall der Alarmierung")
	_ = fs.Parse(args)

	if *signKey == "" {
		*signKey = filepath.Join(*data, "signing.key")
	}
	if *agentsFile == "" {
		*agentsFile = filepath.Join(*data, "agents.json")
	}
	px, err := ratelimit.ParseProxies(*proxies)
	if err != nil {
		log.Fatalf("-trusted-proxy: %v", err)
	}
	cfg := status.DefaultConfig()
	cfg.StaleAfter = *stale
	if cfg, err = status.LoadThresholds(*thresholds, cfg); err != nil {
		log.Fatalf("-thresholds: %v", err)
	}
	st, err := store.OpenSQLite(filepath.Join(*data, "sitewatch.db"), cfg.Retention)
	if err != nil {
		log.Fatal(err)
	}
	// Einmaliger Import der Dateien des früheren Speichers; sie bleiben als Sicherung liegen.
	if n, err := st.ImportFiles(*data, func(e store.Entry) int {
		return int(status.Evaluate(&e.Report, e.ReceivedAt, e.ReceivedAt, cfg).Level) // Eingangszeit = jetzt, "keine Meldung" entfällt
	}); err != nil {
		log.Fatalf("Import der bisherigen Dateien: %v", err)
	} else if n > 0 {
		log.Printf("%d Reports aus %s importiert", n, *data)
	}
	go func() { // verdichten und aufräumen: beim Start und danach täglich
		for {
			if err := st.Maintain(time.Now()); err != nil {
				log.Printf("Verlauf verdichten: %v", err)
			}
			time.Sleep(24 * time.Hour)
		}
	}()
	if *useEOL {
		ep := &eol.Provider{CacheFile: eol.CachePath(*data)}
		cfg.EOL = ep
		go ep.Run(context.Background(), 24*time.Hour)
	}
	if to := split(*alertTo); len(to) > 0 {
		if *alertFrom == "" || !strings.Contains(*alertSMTP, ":") {
			log.Fatal("-alert-to braucht -alert-from und -alert-smtp als host:port")
		}
		w := &alert.Watcher{Store: st, Config: cfg, Now: time.Now,
			State: st, Send: alert.SMTP(*alertSMTP, os.Getenv("SITEWATCH_SMTP_USER"), os.Getenv("SITEWATCH_SMTP_PASSWORD"), *alertFrom, to)}
		go w.Run(context.Background(), *alertEvery)
	} else {
		log.Print("Hinweis: keine Alarmierung (-alert-to nicht gesetzt)")
	}
	srv := web.New(st, *tokens, cfg)
	srv.Version = version
	if srv.SignKey, err = sign.LoadOrCreateKey(*signKey); err != nil {
		log.Fatalf("-signing-key: %v", err)
	}
	srv.Agents, srv.PublicURL = agents.Open(*agentsFile), *publicURL
	srv.Jobs = st
	log.Printf("Öffentlicher Schlüssel für die Agenten: %s", sign.EncodePublicKey(srv.SignKey.Public().(ed25519.PublicKey)))
	handler := srv.Handler()
	if *noLogin {
		log.Print("WARNUNG: Oberfläche läuft ohne Login (-no-login)")
	} else {
		acc := login.Access{Subjects: split(*sub), Emails: split(*email), Groups: split(*group)}
		handler = withLogin(srv, acc)
	}
	hs := &http.Server{
		Addr: *addr, Handler: accessLog(limited(handler, px), px),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
	}
	log.Printf("Sitewatch %s läuft auf http://%s", version, *addr)
	log.Fatal(hs.ListenAndServe())
}

// pubkey gibt den öffentlichen Schlüssel für die Datei der Agenten aus (der private wird bei Bedarf angelegt).
func pubkey(args []string) {
	fs := flag.NewFlagSet("pubkey", flag.ExitOnError)
	data := fs.String("data", "./data", "Datenverzeichnis")
	key := fs.String("signing-key", "", "Schlüsseldatei (Standard: <data>/signing.key)")
	_ = fs.Parse(args)
	if *key == "" {
		*key = filepath.Join(*data, "signing.key")
	}
	priv, err := sign.LoadOrCreateKey(*key)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(sign.EncodePublicKey(priv.Public().(ed25519.PublicKey)))
}

func token(args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	tokens := fs.String("tokens", "./tokens.json", "Datei mit Token-Hashes der Agenten")
	name := fs.String("name", "", "Anzeigename für den Konfigurationsblock (Standard: site-id)")
	kind := fs.String("kind", "contao", `Art für den Konfigurationsblock: "contao", "php" oder "host"`)
	siteURL := fs.String("url", "", "öffentliche URL für den Konfigurationsblock")
	path := fs.String("path", "", "Projektverzeichnis für den Konfigurationsblock")
	rotate := fs.Bool("rotate", false, "Token einer bereits registrierten Site ersetzen (das alte Token wird ungültig)")
	remove := fs.Bool("remove", false, "Token der Site löschen (der Agent mit diesem Token bekommt 401); z. B. vor der Umstellung auf einen Agenten im Dashboard")
	_ = fs.Parse(args)
	if fs.NArg() != 1 || (*remove && *rotate) || (*kind != "contao" && *kind != "php" && *kind != "host") {
		fmt.Fprintln(os.Stderr, "Verwendung: sitewatch token [-tokens datei] [-kind contao|php|host] [-name n] [-url u] [-path p] [-rotate | -remove] <site-id>")
		os.Exit(2)
	}
	id := fs.Arg(0)
	if *remove {
		ok, err := auth.Remove(*tokens, id)
		if err != nil {
			log.Fatal(err)
		}
		if !ok {
			log.Fatalf("Site %q hat kein Token.", id)
		}
		fmt.Fprintf(os.Stderr, "Token für %q gelöscht. Ein Agent, der es noch benutzt, wird mit 401 abgewiesen.\n", id)
		return
	}
	if known, err := auth.Load(*tokens); err != nil {
		log.Fatal(err)
	} else if _, ok := known[id]; ok && !*rotate {
		// Ein erneuter Aufruf würde das Token des laufenden Agenten stillschweigend ungültig machen (401).
		log.Fatalf("Site %q hat schon ein Token. Mit -rotate ersetzen (das alte wird ungültig, der Agent braucht das neue).", id)
	}
	t, err := auth.Add(*tokens, id)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(t) // nur Token auf stdout, damit es sich in Skripten weiterverwenden lässt
	fmt.Fprintf(os.Stderr, "Token für %q angelegt. Es wird nur einmal angezeigt.\n\nBlock für die Agent-Konfiguration (in \"sites\" einfügen):\n%s\n", id, configBlock(id, *name, *kind, *siteURL, *path, t))
}

// configBlock liefert den Eintrag für "sites" der Agent-Konfiguration. Fehlt bei einer Site der Pfad, steht ein
// bewusst ungültiger Platzhalter darin, den der Agent beim Start ablehnt, statt still einen falschen Wert zu verwenden.
func configBlock(id, name, kind, siteURL, path, token string) string {
	if name == "" {
		name = id
	}
	if path == "" && kind != "host" {
		path = "<PFAD EINTRAGEN>"
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // sonst wird der Platzhalter <PFAD EINTRAGEN> als \u003c… ausgegeben
	enc.SetIndent("    ", "  ")
	_ = enc.Encode(struct { // Encode kann für diese Struktur nicht scheitern
		ID    string `json:"id"`
		Name  string `json:"name"`
		Kind  string `json:"kind"`
		URL   string `json:"url,omitempty"`
		Path  string `json:"path,omitempty"`
		Token string `json:"token"`
	}{id, name, kind, siteURL, path, token})
	return "    " + strings.TrimSpace(buf.String())
}

// withLogin schützt die Oberfläche per OIDC. Geheimnisse kommen aus der Umgebung, nicht aus Flags (sichtbar in der Prozessliste):
// SITEWATCH_OIDC_ISSUER, SITEWATCH_OIDC_CLIENT_ID, SITEWATCH_OIDC_CLIENT_SECRET, SITEWATCH_OIDC_REDIRECT_URL,
// SITEWATCH_SESSION_KEY (hex, mind. 32 Byte, z. B. `openssl rand -hex 32`).
func withLogin(srv *web.Server, acc login.Access) http.Handler {
	if err := acc.Validate(); err != nil {
		log.Fatal(err)
	}
	env := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			log.Fatalf("Umgebungsvariable %s fehlt", k)
		}
		return v
	}
	key, err := hex.DecodeString(env("SITEWATCH_SESSION_KEY"))
	if err != nil {
		log.Fatal("SITEWATCH_SESSION_KEY ist kein Hex-String")
	}
	redirect := env("SITEWATCH_OIDC_REDIRECT_URL")
	sessions, err := login.NewSessions(key, 12*time.Hour, strings.HasPrefix(redirect, "https://"))
	if err != nil {
		log.Fatal(err)
	}
	sessions.Allow = acc.Allows // Rechte bei jedem Zugriff neu prüfen, nicht nur beim Login
	lg, err := login.New(login.Config{
		Issuer: env("SITEWATCH_OIDC_ISSUER"), ClientID: env("SITEWATCH_OIDC_CLIENT_ID"),
		ClientSecret: env("SITEWATCH_OIDC_CLIENT_SECRET"), RedirectURL: redirect,
	}, func(w http.ResponseWriter, r *http.Request, id login.Identity) {
		if !acc.Allows(id) {
			log.Printf("login: Zugriff verweigert für %q (Gruppen im Token: %q)", id.Subject, id.Groups)
			http.Error(w, "kein Zugriff", http.StatusForbidden)
			return
		}
		sessions.Set(w, id)
		log.Printf("login: angemeldet sub=%q email=%q", id.Subject, id.Email)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	if err != nil {
		log.Fatal(err)
	}
	srv.Protect = sessions.Require
	srv.Who = func(r *http.Request) string { // wer einen Auftrag freigibt, wird mit gespeichert
		id, ok := sessions.Get(r)
		switch {
		case !ok:
			return ""
		case id.Email != "":
			return id.Email
		}
		return id.Subject
	}
	mux := http.NewServeMux()
	lg.Register(mux)
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if id, ok := sessions.Get(r); ok {
			log.Printf("login: abgemeldet sub=%q", id.Subject)
		}
		sessions.Logout(w, r)
	})
	mux.Handle("/", srv.Handler())
	return mux
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// limited begrenzt Login-Aufrufe und die Agent-API pro Client-IP. Die Grenzen sind bewusst großzügig
// (Agenten melden etwa alle 5 Minuten, ein Spool von bis zu 100 Reports darf als Schub nachgesendet werden).
func limited(next http.Handler, px []netip.Prefix) http.Handler {
	loginH := ratelimit.New(10, 6*time.Second).Middleware(px, next)       // 10 sofort, dann 10/min
	apiH := ratelimit.New(120, 500*time.Millisecond).Middleware(px, next) // 120 sofort (Spool bis 100), dann 120/min
	jobH := ratelimit.New(300, 250*time.Millisecond).Middleware(px, next) // Long-Poll alle ~20 s, während eines Auftrags Ausgabe-Blöcke ca. 2/s
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login", "/callback":
			loginH.ServeHTTP(w, r)
		case "/api/v1/reports", "/api/v1/agent/config":
			apiH.ServeHTTP(w, r)
		default:
			if strings.HasPrefix(r.URL.Path, "/api/v1/agent/jobs") {
				jobH.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

// Unwrap macht Flush und Zeitlimits für http.ResponseController erreichbar (Live-Ansicht der Aufträge).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// accessLog protokolliert jede Anfrage (außer /healthz) mit Client-IP, Status und Dauer.
func accessLog(next http.Handler, px []netip.Prefix) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		log.Printf("http: %s %s -> %d (%s) von %s", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond), ratelimit.ClientIP(r, px))
	})
}
