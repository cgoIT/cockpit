package login

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// stubIdP ist ein minimaler OIDC-Provider nur für Tests (keine echte Anmeldung).
type stubIdP struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	challenge string // aus dem authorize-Request des Tests
	nonce     string
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newStub(t *testing.T) *stubIdP {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	s := &stubIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": s.srv.URL, "authorization_endpoint": s.srv.URL + "/authorize",
			"token_endpoint": s.srv.URL + "/token", "jwks_uri": s.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if b64(sum[:]) != s.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "x", "token_type": "Bearer", "id_token": s.idToken(s.nonce)})
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubIdP) idToken(nonce string) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	p, _ := json.Marshal(map[string]any{
		"iss": s.srv.URL, "aud": "cid", "sub": "user-1", "email": "a@example.org", "email_verified": true, "groups": []string{"sitewatch"},
		"nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	in := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	return in + "." + b64(sig)
}

// begin startet den Flow und liefert Cookie und state.
func begin(t *testing.T, l *Login, s *stubIdP) (*http.Cookie, string) {
	t.Helper()
	mux := http.NewServeMux()
	l.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || rec.Code != http.StatusFound {
		t.Fatalf("kein Redirect: %d %v", rec.Code, err)
	}
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" {
		t.Fatalf("PKCE/nonce fehlen: %v", q)
	}
	s.challenge, s.nonce = q.Get("code_challenge"), q.Get("nonce")
	return rec.Result().Cookies()[0], q.Get("state")
}

func callback(l *Login, c *http.Cookie, state string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	l.Register(mux)
	req := httptest.NewRequest("GET", "/callback?code=c&state="+url.QueryEscape(state), nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func setup(t *testing.T) (*Login, *stubIdP, *[]Identity) {
	s := newStub(t)
	var got []Identity
	l, err := New(Config{Issuer: s.srv.URL, ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://sitewatch.test/callback"},
		func(w http.ResponseWriter, _ *http.Request, id Identity) {
			got = append(got, id)
			w.WriteHeader(http.StatusNoContent)
		})
	if err != nil {
		t.Fatal(err)
	}
	return l, s, &got
}

func TestFlowSuccess(t *testing.T) {
	l, s, got := setup(t)
	c, state := begin(t, l, s)
	if rec := callback(l, c, state); rec.Code != http.StatusNoContent {
		t.Fatalf("Code %d: %s", rec.Code, rec.Body)
	}
	if len(*got) != 1 || (*got)[0].Subject != "user-1" || (*got)[0].Groups[0] != "sitewatch" {
		t.Fatalf("Identität: %+v", *got)
	}
}

func TestStartCookieAndNeedsHTTPS(t *testing.T) {
	l, s, _ := setup(t)
	c, _ := begin(t, l, s)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/callback" || c.MaxAge != 600 {
		t.Errorf("Cookie-Attribute: %+v", c)
	}
	if _, err := New(Config{Issuer: "http://idp.example", RedirectURL: "https://c.example/callback"}, func(http.ResponseWriter, *http.Request, Identity) {}); err == nil {
		t.Error("http-Issuer wurde akzeptiert")
	}
	if _, err := New(Config{Issuer: s.srv.URL, RedirectURL: "https://c.example/callback"}, nil); err == nil {
		t.Error("nil OnSuccess wurde akzeptiert")
	}
}

func TestProviderErrorAndBadPKCE(t *testing.T) {
	l, s, got := setup(t)
	c, state := begin(t, l, s)
	mux := http.NewServeMux()
	l.Register(mux)
	req := httptest.NewRequest("GET", "/callback?error=access_denied&state="+url.QueryEscape(state), nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("?error=: %d", rec.Code)
	}
	s.challenge = "anderer-challenge" // Provider erwartet anderen Verifier
	if rec := callback(l, c, state); rec.Code != http.StatusUnauthorized {
		t.Errorf("falscher PKCE-Verifier: %d", rec.Code)
	}
	if len(*got) != 0 {
		t.Errorf("OnSuccess trotz Fehler: %+v", *got)
	}
}

func TestFlowRejects(t *testing.T) {
	l, s, got := setup(t)
	c, state := begin(t, l, s)
	if rec := callback(l, c, "falsch"); rec.Code != http.StatusBadRequest {
		t.Errorf("falscher state: %d", rec.Code)
	}
	if rec := callback(l, nil, state); rec.Code != http.StatusBadRequest {
		t.Errorf("ohne Cookie: %d", rec.Code)
	}
	s.nonce = "fremd" // Token für anderen Flow
	if rec := callback(l, c, state); rec.Code != http.StatusUnauthorized {
		t.Errorf("falsche nonce: %d", rec.Code)
	}
	if len(*got) != 0 {
		t.Errorf("OnSuccess trotz Fehler: %+v", *got)
	}
}
