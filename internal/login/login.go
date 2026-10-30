// Package login führt den OIDC-Login (Authorization Code mit PKCE) gegen einen Identity Provider wie Authentik aus.
// Er liefert nur die geprüfte Identität; die Session legt der Aufrufer in OnSuccess an.
package login

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const flowCookie = "cockpit_oidc"

// Config beschreibt den Client beim Identity Provider.
type Config struct {
	Issuer, ClientID, ClientSecret string
	RedirectURL                    string // z. B. https://cockpit.example/callback
}

// Identity ist das geprüfte Ergebnis eines Logins.
// Email ist nur gesetzt, wenn der Provider sie als verifiziert meldet; Freigaben sollten bevorzugt über Subject oder Groups laufen.
type Identity struct {
	Subject, Email string
	Groups         []string
}

type Login struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	secure   bool
	// OnSuccess wird nach erfolgreicher Prüfung aufgerufen und muss die Antwort abschließen.
	OnSuccess func(w http.ResponseWriter, r *http.Request, id Identity)
}

// New lädt die Provider-Metadaten (Netzwerkzugriff auf den Issuer). Issuer und Redirect-URL müssen https sein,
// nur Loopback-Adressen dürfen http nutzen (lokale Entwicklung).
func New(cfg Config, onSuccess func(http.ResponseWriter, *http.Request, Identity)) (*Login, error) {
	if onSuccess == nil {
		return nil, errors.New("OnSuccess fehlt")
	}
	for _, u := range []string{cfg.Issuer, cfg.RedirectURL} {
		if err := needHTTPS(u); err != nil {
			return nil, err
		}
	}
	// Der Kontext lebt so lange wie der Provider (go-oidc lädt damit später die Schlüssel nach); das Timeout steckt im Client.
	ctx := oidc.ClientContext(context.Background(), &http.Client{Timeout: 15 * time.Second})
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	return &Login{
		oauth: oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: p.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier:  p.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		secure:    strings.HasPrefix(cfg.RedirectURL, "https://"),
		OnSuccess: onSuccess,
	}, nil
}

func needHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch h := u.Hostname(); {
	case u.Scheme == "https":
	case u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1"):
	default:
		return fmt.Errorf("%q muss https sein", raw)
	}
	return nil
}

// Register hängt /login und /callback an den Mux.
func (l *Login) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", l.start)
	mux.HandleFunc("GET /callback", l.callback)
}

func rnd() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // ohne Zufall ist kein sicherer Login möglich
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (l *Login) start(w http.ResponseWriter, r *http.Request) {
	state, nonce, verifier := rnd(), rnd(), oauth2.GenerateVerifier()
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, &http.Cookie{
		Name: flowCookie, Value: state + "." + nonce + "." + verifier, Path: "/callback",
		MaxAge: 600, HttpOnly: true, Secure: l.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, l.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (l *Login) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(flowCookie)
	// Cookie sofort entwerten: jeder Flow ist einmalig.
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Path: "/callback", MaxAge: -1, HttpOnly: true, Secure: l.secure})
	parts := []string(nil)
	if err == nil {
		parts = strings.Split(c.Value, ".")
	}
	q := r.URL.Query()
	if len(parts) != 3 || subtle.ConstantTimeCompare([]byte(parts[0]), []byte(q.Get("state"))) != 1 {
		http.Error(w, "Login-Ablauf ungültig oder abgelaufen", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		log.Printf("login: Provider meldet Fehler %q", e)
		http.Error(w, "Anmeldung abgelehnt", http.StatusForbidden)
		return
	}
	id, err := l.exchange(r.Context(), q.Get("code"), parts[1], parts[2])
	if err != nil {
		log.Printf("login: %v", err)
		http.Error(w, "Anmeldung fehlgeschlagen", http.StatusUnauthorized)
		return
	}
	l.OnSuccess(w, r, id)
}

func (l *Login) exchange(ctx context.Context, code, nonce, verifier string) (Identity, error) {
	if code == "" {
		return Identity{}, errors.New("kein code")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tok, err := l.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("Token-Austausch: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, errors.New("Token-Antwort enthält kein id_token (Scope openid im Provider freigegeben?)")
	}
	idt, err := l.verifier.Verify(ctx, raw)
	if err != nil {
		if n := strings.Count(raw, ".") + 1; n != 3 {
			return Identity{}, fmt.Errorf("ID-Token: %w (id_token hat %d statt 3 Teile; 5 = verschlüsselt, Encryption Key im Provider entfernen)", err, n)
		}
		return Identity{}, fmt.Errorf("ID-Token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return Identity{}, errors.New("nonce stimmt nicht")
	}
	var cl struct {
		Email         string   `json:"email"`
		EmailVerified bool     `json:"email_verified"`
		Groups        []string `json:"groups"`
	}
	if err := idt.Claims(&cl); err != nil {
		return Identity{}, err
	}
	if !cl.EmailVerified {
		cl.Email = ""
	}
	return Identity{Subject: idt.Subject, Email: cl.Email, Groups: cl.Groups}, nil
}
