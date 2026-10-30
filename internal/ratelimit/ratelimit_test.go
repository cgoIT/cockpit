package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	l := New(2, time.Second)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }
	a, b := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow(a); !ok {
			t.Fatalf("Burst %d abgelehnt", i)
		}
	}
	if ok, wait := l.Allow(a); ok || wait <= 0 {
		t.Errorf("dritte Anfrage: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow(b); !ok {
		t.Error("andere IP betroffen")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow(a); !ok {
		t.Error("nach Wartezeit weiterhin gesperrt")
	}
}

func req(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for _, x := range xff {
		r.Header.Add("X-Forwarded-For", x)
	}
	return r
}

func TestClientIP(t *testing.T) {
	px, err := ParseProxies("10.0.0.0/8, 172.18.0.2")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"ohne Proxy zählt RemoteAddr, Header wird ignoriert", req("203.0.113.5:1000", "1.2.3.4"), "203.0.113.5"},
		{"Proxy: letzter Eintrag", req("172.18.0.2:1", "198.51.100.7"), "198.51.100.7"},
		{"Fälschung links wird übergangen", req("172.18.0.2:1", "9.9.9.9, 198.51.100.7"), "198.51.100.7"},
		{"Proxy-Kette", req("172.18.0.2:1", "198.51.100.7, 10.1.1.1"), "198.51.100.7"},
		{"Müll im Header", req("172.18.0.2:1", "unsinn"), "172.18.0.2"},
		{"IPv6 auf /64", req("[2001:db8:1:2:aaaa::1]:1"), "2001:db8:1:2::"},
	}
	for _, c := range cases {
		if got := ClientIP(c.r, px).String(); got != c.want {
			t.Errorf("%s: %s, erwartet %s", c.name, got, c.want)
		}
	}
	if _, err := ParseProxies("kein-ip"); err == nil {
		t.Error("ungültiger Proxy akzeptiert")
	}
}

func TestMiddleware(t *testing.T) {
	h := New(1, time.Minute).Middleware(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req("203.0.113.5:1"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("erste: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req("203.0.113.5:2"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("zweite: %d %v", rec.Code, rec.Header())
	}
}

func TestSweepAndCap(t *testing.T) {
	l := New(1, time.Second)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }
	l.maxKeys = 5000
	for i := 0; i < 5000; i++ {
		l.Allow(netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}))
	}
	if ok, _ := l.Allow(netip.MustParseAddr("192.0.2.1")); ok {
		t.Error("Obergrenze wird nicht durchgesetzt")
	}
	now = now.Add(2 * time.Minute) // alle Einträge sind wieder voll und werden entfernt
	if ok, _ := l.Allow(netip.MustParseAddr("192.0.2.1")); !ok {
		t.Error("nach der Bereinigung weiterhin gesperrt")
	}
	if len(l.b) != 1 {
		t.Errorf("Map nicht bereinigt: %d Einträge", len(l.b))
	}
}
