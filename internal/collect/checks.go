package collect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// tlsExpiry liefert das Ablaufdatum des Zertifikats, das die URL ausliefert.
// verr ist gesetzt, wenn Kette oder Hostname nicht stimmen; Ablauf allein zählt nicht (das meldet das Datum).
func tlsExpiry(ctx context.Context, rawURL string) (expires time.Time, verr, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return time.Time{}, nil, err
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := tls.Dialer{Config: &tls.Config{
		ServerName: u.Hostname(),
		// Handshake ohne Prüfung, damit auch ein abgelaufenes Zertifikat lesbar ist; geprüft wird unten selbst.
		InsecureSkipVerify: true,
	}}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return time.Time{}, nil, err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return time.Time{}, nil, errors.New("kein Zertifikat erhalten")
	}
	opts := x509.VerifyOptions{DNSName: u.Hostname(), Intermediates: x509.NewCertPool()}
	for _, c := range certs[1:] {
		opts.Intermediates.AddCert(c)
	}
	if _, err := certs[0].Verify(opts); err != nil {
		var ci x509.CertificateInvalidError
		if !errors.As(err, &ci) || ci.Reason != x509.Expired {
			verr = err
		}
	}
	return certs[0].NotAfter.UTC(), verr, nil
}

// newestMod liefert die jüngste Änderungszeit und die Anzahl der Dateien in dir, deren Name zur Maske passt.
// Leere Maske = alle Dateien. Bei 0 Treffern ist die Zeit leer.
// shortcut: nur die oberste Ebene und keine Symlinks (z. B. latest.tar.gz), bei tieferer Verschachtelung oder Symlink-Rotation erweitern.
func newestMod(dir, glob string) (newest time.Time, count int, err error) {
	if glob == "" {
		glob = "*"
	}
	if _, err := filepath.Match(glob, ""); err != nil {
		return time.Time{}, 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, 0, err
	}
	for _, e := range entries {
		if ok, _ := filepath.Match(glob, e.Name()); !ok {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		count++
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest.UTC(), count, nil
}
