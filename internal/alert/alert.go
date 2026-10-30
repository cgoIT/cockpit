// Package alert verschickt eine E-Mail, sobald eine Site kritisch wird, und eine Entwarnung, wenn sie es nicht mehr ist.
// Gemeldet wird nur der Wechsel des Zustands (Deduplizierung), nicht jeder Prüfzyklus.
package alert

import (
	"context"
	"fmt"
	"log"
	"net/smtp"
	"strings"
	"time"

	"cockpit/internal/status"
	"cockpit/internal/store"
)

// Sender verschickt eine Nachricht. Ein Fehler führt dazu, dass der Versuch im nächsten Zyklus wiederholt wird.
type Sender func(subject, body string) error

// State hält fest, für welche Sites ein Alarm offen ist (z. B. store.SQLite), damit ein Neustart nicht erneut alarmiert.
type State interface {
	AlertState() (map[string]bool, error)
	SetAlerted(siteID string, on bool) error
}

// Watcher prüft periodisch den jeweils letzten Report aller Sites.
// Ohne State liegt der Zustand nur im Speicher: Nach einem Neustart wird für jede dann kritische Site erneut alarmiert.
// shortcut: keine Erinnerung bei lang anhaltendem Alarm, bei Bedarf Zeitpunkt des letzten Alarms in den State aufnehmen.
type Watcher struct {
	Store  store.Store
	Config status.Config
	Send   Sender
	Now    func() time.Time
	State  State // optional

	crit map[string]bool // Site-ID -> Alarm wurde versendet
}

// Check macht einen Durchlauf. Eine Site ohne neue Meldung wird hier über StaleAfter kritisch,
// deshalb muss das unabhängig vom Eingang von Reports laufen.
func (w *Watcher) Check() {
	if w.crit == nil {
		w.crit = map[string]bool{}
		if w.State != nil {
			if saved, err := w.State.AlertState(); err != nil {
				log.Printf("alert: Zustand lesen: %v", err)
			} else {
				w.crit = saved
			}
		}
	}
	entries, err := w.Store.Latest()
	if err != nil {
		log.Printf("alert: Store: %v", err)
		return
	}
	now := w.Now()
	for _, e := range entries {
		id := e.Report.Site.ID
		res := status.Evaluate(&e.Report, e.ReceivedAt, now, w.Config)
		name := e.Report.Site.Name
		if name == "" {
			name = id
		}
		switch {
		case res.Level == status.Crit && !w.crit[id]:
			body := "Kritisch:\n- " + strings.Join(res.Reasons, "\n- ") + "\n"
			if w.Send("Cockpit: "+name+" ist kritisch", body) == nil {
				w.crit[id] = true
				w.persist(id, true)
			}
		case res.Level != status.Crit && w.crit[id]:
			if w.Send("Cockpit: "+name+" ist wieder in Ordnung", "Status: "+res.Level.String()+"\n") == nil {
				delete(w.crit, id)
				w.persist(id, false)
			}
		}
	}
}

func (w *Watcher) persist(id string, on bool) {
	if w.State == nil {
		return
	}
	if err := w.State.SetAlerted(id, on); err != nil {
		log.Printf("alert: Zustand speichern: %v", err)
	}
}

// Run ruft Check im Intervall auf, bis ctx endet.
func (w *Watcher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		w.Check()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SMTP liefert einen Sender über net/smtp (STARTTLS, wenn der Server es anbietet; PlainAuth nur über TLS oder localhost).
// shortcut: kein Port 465 mit implizitem TLS, bei Bedarf crypto/tls + smtp.NewClient ergänzen.
func SMTP(addr, user, password, from string, to []string) Sender {
	host := addr[:strings.LastIndex(addr, ":")]
	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, password, host)
	}
	return func(subject, body string) error {
		msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
			header(from), header(strings.Join(to, ", ")), header(subject), body)
		err := smtp.SendMail(addr, auth, from, to, []byte(msg))
		if err != nil {
			log.Printf("alert: E-Mail an %v: %v", to, err)
		}
		return err
	}
}

// header entfernt Zeilenumbrüche, damit der vom Agenten gelieferte Site-Name keine Header einschleusen kann.
func header(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}
