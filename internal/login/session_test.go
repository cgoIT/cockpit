package login

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newSessions(t *testing.T) (*Sessions, *time.Time) {
	t.Helper()
	s, err := NewSessions([]byte(strings.Repeat("k", 32)), time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	return s, &now
}

func issue(s *Sessions) *http.Cookie {
	rec := httptest.NewRecorder()
	s.Set(rec, Identity{Subject: "u1", Email: "a@example.org", Groups: []string{"cockpit"}})
	return rec.Result().Cookies()[0]
}

func reqWith(c *http.Cookie) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if c != nil {
		r.AddCookie(c)
	}
	return r
}

func TestSessionRoundtripAndAttributes(t *testing.T) {
	s, _ := newSessions(t)
	c := issue(s)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 3600 {
		t.Errorf("Cookie-Attribute: %+v", c)
	}
	id, ok := s.Get(reqWith(c))
	if !ok || id.Subject != "u1" || id.Groups[0] != "cockpit" {
		t.Fatalf("Get: %+v %v", id, ok)
	}
}

func TestSessionRejects(t *testing.T) {
	s, now := newSessions(t)
	c := issue(s)

	p, sig, _ := strings.Cut(c.Value, ".")
	tampered := &http.Cookie{Name: c.Name, Value: p + "x." + sig}
	if _, ok := s.Get(reqWith(tampered)); ok {
		t.Error("manipulierte Nutzdaten akzeptiert")
	}
	if _, ok := s.Get(reqWith(&http.Cookie{Name: c.Name, Value: p})); ok {
		t.Error("ohne Signatur akzeptiert")
	}
	other, _ := NewSessions([]byte(strings.Repeat("z", 32)), time.Hour, true)
	other.Now = s.Now
	if _, ok := other.Get(reqWith(c)); ok {
		t.Error("fremder Schlüssel akzeptiert")
	}
	if _, ok := s.Get(reqWith(nil)); ok {
		t.Error("ohne Cookie akzeptiert")
	}
	*now = now.Add(time.Hour)
	if _, ok := s.Get(reqWith(c)); ok {
		t.Error("abgelaufene Sitzung akzeptiert")
	}
}

func TestNewSessionsValidates(t *testing.T) {
	if _, err := NewSessions([]byte("kurz"), time.Hour, true); err == nil {
		t.Error("kurzer Schlüssel akzeptiert")
	}
	if _, err := NewSessions([]byte(strings.Repeat("k", 32)), 0, true); err == nil {
		t.Error("TTL 0 akzeptiert")
	}
}

func TestRequire(t *testing.T) {
	s, _ := newSessions(t)
	h := s.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, reqWith(nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Errorf("anonym GET: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonym POST: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, reqWith(issue(s)))
	if rec.Code != http.StatusNoContent {
		t.Errorf("angemeldet: %d", rec.Code)
	}
}

func TestSessionAllowRechecked(t *testing.T) {
	s, _ := newSessions(t)
	c := issue(s)
	s.Allow = func(id Identity) bool { return id.Subject != "u1" }
	if _, ok := s.Get(reqWith(c)); ok {
		t.Error("entzogener Zugriff gilt weiter")
	}
}

func TestLogoutClearsCookieWithoutRedirect(t *testing.T) {
	s, _ := newSessions(t)
	rec := httptest.NewRecorder()
	s.Logout(rec, httptest.NewRequest(http.MethodPost, "/logout", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Fatalf("kein Redirect erwartet: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		cleared = cleared || (c.Name == sessionCookie && c.MaxAge < 0)
	}
	if !cleared {
		t.Error("Cookie nicht gelöscht")
	}
}
