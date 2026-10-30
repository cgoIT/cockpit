// Package ratelimit begrenzt Anfragen pro Client-IP (Token-Bucket, im Speicher).
package ratelimit

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limiter erlaubt burst Anfragen sofort und danach eine je every.
// shortcut: Zustand nur im Speicher und pro Prozess, bei mehreren Cockpit-Instanzen in den gemeinsamen Store verlagern.
type Limiter struct {
	burst int
	every time.Duration
	Now   func() time.Time

	mu        sync.Mutex
	b         map[netip.Addr]bucket
	lastSweep time.Time
	maxKeys   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

func New(burst int, every time.Duration) *Limiter {
	return &Limiter{burst: burst, every: every, Now: time.Now, b: map[netip.Addr]bucket{}, maxKeys: 100000}
}

// Allow verbraucht ein Token für key. Sonst false und die Wartezeit bis zum nächsten Token.
func (l *Limiter) Allow(key netip.Addr) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	if len(l.b) > 4096 && now.Sub(l.lastSweep) > time.Minute { // höchstens einmal pro Minute: Leerlauf-Einträge (wieder voll) entfernen
		l.lastSweep = now
		for k, v := range l.b {
			if now.Sub(v.last) > time.Duration(l.burst)*l.every {
				delete(l.b, k)
			}
		}
	}
	v, ok := l.b[key]
	if !ok {
		// shortcut: harte Obergrenze, neue Adressen werden darüber abgewiesen (auch legitime), bei Bedarf auf einen gemeinsamen Bucket zurückfallen.
		if len(l.b) >= l.maxKeys {
			return false, l.every
		}
		v = bucket{tokens: float64(l.burst), last: now}
	}
	v.tokens = min(float64(l.burst), v.tokens+float64(now.Sub(v.last))/float64(l.every))
	v.last = now
	if v.tokens < 1 {
		l.b[key] = v
		return false, time.Duration((1 - v.tokens) * float64(l.every))
	}
	v.tokens--
	l.b[key] = v
	return true, 0
}

// ParseProxies liest kommagetrennte IPs oder CIDRs vertrauenswürdiger Proxys.
func ParseProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if pf, err := netip.ParsePrefix(p); err == nil {
			out = append(out, pf)
			continue
		}
		a, err := netip.ParseAddr(p)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func trusted(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP liefert die Client-Adresse. Nur wenn die Verbindung von einem vertrauenswürdigen Proxy kommt,
// wird X-Forwarded-For ausgewertet: von rechts der erste Eintrag, der selbst kein vertrauenswürdiger Proxy ist.
// IPv6 wird auf das /64-Präfix reduziert, sonst umgeht ein Angreifer das Limit mit beliebigen Adressen.
func ClientIP(r *http.Request, proxies []netip.Prefix) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, _ := netip.ParseAddr(host)
	ip = ip.Unmap()
	if trusted(proxies, ip) {
		xs := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		for i := len(xs) - 1; i >= 0; i-- {
			a, err := netip.ParseAddr(strings.TrimSpace(xs[i]))
			if err != nil {
				break // unlesbarer Eintrag: nicht raten, die Proxy-Adresse zählt
			}
			if ip = a.Unmap(); !trusted(proxies, ip) {
				break
			}
		}
	}
	if ip.Is6() {
		pf, _ := ip.Prefix(64)
		return pf.Addr()
	}
	return ip
}

// Middleware antwortet bei Überschreitung mit 429 und Retry-After.
func (l *Limiter) Middleware(proxies []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.Allow(ClientIP(r, proxies)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			http.Error(w, "zu viele Anfragen", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
