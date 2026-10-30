package login

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const sessionCookie = "sitewatch_session"

// Sessions verwaltet Login-Sitzungen als signiertes Cookie (HMAC-SHA256, ohne serverseitigen Speicher).
// shortcut: keine serverseitige Sperrliste, eine Sitzung lässt sich vor Ablauf nur durch Wechsel des Schlüssels beenden; bei Bedarf in den SQLite-Store verlagern.
type Sessions struct {
	key    []byte
	ttl    time.Duration
	secure bool
	Now    func() time.Time
	// Allow prüft die Identität bei jedem Zugriff erneut (nil = keine Prüfung), damit entzogene Rechte sofort wirken.
	Allow func(Identity) bool
}

type claims struct {
	Sub    string   `json:"sub"`
	Email  string   `json:"email,omitempty"`
	Groups []string `json:"groups,omitempty"`
	Exp    int64    `json:"exp"`
}

// NewSessions verlangt einen Schlüssel von mindestens 32 Byte. secure setzt das Secure-Attribut (immer true außer lokaler Entwicklung).
func NewSessions(key []byte, ttl time.Duration, secure bool) (*Sessions, error) {
	if len(key) < 32 {
		return nil, errors.New("Session-Schlüssel muss mindestens 32 Byte lang sein")
	}
	if ttl <= 0 {
		return nil, errors.New("Session-Laufzeit muss positiv sein")
	}
	return &Sessions{key: key, ttl: ttl, secure: secure, Now: time.Now}, nil
}

func (s *Sessions) sign(payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Set legt die Sitzung für id an.
func (s *Sessions) Set(w http.ResponseWriter, id Identity) {
	b, _ := json.Marshal(claims{Sub: id.Subject, Email: id.Email, Groups: id.Groups, Exp: s.Now().Add(s.ttl).Unix()})
	p := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: p + "." + s.sign(p), Path: "/",
		MaxAge: int(s.ttl.Seconds()), HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// Clear beendet die Sitzung im Browser.
func (s *Sessions) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
}

// Get liefert die Identität, wenn das Cookie vorhanden, unverfälscht und nicht abgelaufen ist.
func (s *Sessions) Get(r *http.Request) (Identity, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Identity{}, false
	}
	p, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(p))) {
		return Identity{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	var cl claims
	if err != nil || json.Unmarshal(raw, &cl) != nil || cl.Sub == "" || s.Now().Unix() >= cl.Exp {
		return Identity{}, false
	}
	id := Identity{Subject: cl.Sub, Email: cl.Email, Groups: cl.Groups}
	if s.Allow != nil && !s.Allow(id) {
		return Identity{}, false
	}
	return id, true
}

// Require lässt nur angemeldete Benutzer durch; sonst Weiterleitung auf /login (GET) bzw. 401.
func (s *Sessions) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.Get(r); ok {
			w.Header().Set("Cache-Control", "private, no-store")
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		http.Error(w, "nicht angemeldet", http.StatusUnauthorized)
	})
}

// Logout beendet die Sitzung (nur POST, damit fremde Seiten sie nicht per Link beenden können).
// Es gibt bewusst keinen Redirect: Die CSP "form-action 'self'" gilt auch für die Weiterleitungen nach dem Absenden,
// und /login leitet an den externen Identity-Provider weiter. Der Provider selbst bleibt angemeldet (kein RP-initiated Logout).
func (s *Sessions) Logout(w http.ResponseWriter, _ *http.Request) {
	s.Clear(w)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	io.WriteString(w, `<!doctype html><html lang="de"><meta charset="utf-8"><title>Sitewatch – Abgemeldet</title>
<body style="font:16px system-ui;margin:3rem auto;max-width:28rem"><p>Du bist abgemeldet.</p><p><a href="/login">Erneut anmelden</a></p></body></html>`)
}
