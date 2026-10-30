package alert

import (
	"errors"
	"strings"
	"testing"
	"time"

	"cockpit/internal/report"
	"cockpit/internal/status"
	"cockpit/internal/store"
)

type fakeStore struct{ entries []store.Entry }

func (f *fakeStore) Save(store.Entry, int) error                          { return nil }
func (f *fakeStore) Latest() ([]store.Entry, error)                       { return f.entries, nil }
func (f *fakeStore) Get(string) (store.Entry, bool, error)                { return store.Entry{}, false, nil }
func (f *fakeStore) Baselines(time.Time) (map[string]store.Sample, error) { return nil, nil }
func (f *fakeStore) Days(string) ([]string, error)                        { return nil, nil }
func (f *fakeStore) GetDay(string, string) (store.Entry, bool, error) {
	return store.Entry{}, false, nil
}
func (f *fakeStore) History(string, int) ([]store.Sample, error) { return nil, nil }

type fakeState map[string]bool

func (f fakeState) AlertState() (map[string]bool, error) { return map[string]bool(f), nil }
func (f fakeState) SetAlerted(id string, on bool) error {
	if on {
		f[id] = true
	} else {
		delete(f, id)
	}
	return nil
}

func TestCheckAlertsOnTransitionsOnly(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st := &fakeStore{entries: []store.Entry{{
		Report:     report.Report{Site: report.Site{ID: "a", Name: "Site A"}},
		ReceivedAt: now.Add(-time.Hour), // länger als StaleAfter her = kritisch
	}}}
	var sent []string
	fail := false
	w := &Watcher{Store: st, Config: status.DefaultConfig(), Now: func() time.Time { return now },
		Send: func(subject, _ string) error {
			if fail {
				return errors.New("smtp down")
			}
			sent = append(sent, subject)
			return nil
		}}

	fail = true
	w.Check() // Versand scheitert: nichts vermerkt
	fail = false
	w.Check() // wird wiederholt
	w.Check() // keine Wiederholung
	if len(sent) != 1 || !strings.Contains(sent[0], "kritisch") {
		t.Fatalf("erwartet genau einen Alarm, bekam %q", sent)
	}

	st.entries[0].ReceivedAt = now // frische Meldung
	w.Check()
	w.Check()
	if len(sent) != 2 || !strings.Contains(sent[1], "wieder in Ordnung") {
		t.Fatalf("erwartet Entwarnung, bekam %q", sent)
	}
}

func TestHeaderStripsNewlines(t *testing.T) {
	if got := header("a\r\nBcc: x@evil"); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("Zeilenumbruch im Header: %q", got)
	}
}

func TestStatePersistsAndSuppressesRealert(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st := &fakeStore{entries: []store.Entry{{
		Report:     report.Report{Site: report.Site{ID: "a", Name: "Site A"}},
		ReceivedAt: now.Add(-time.Hour),
	}}}
	state := fakeState{}
	var sent int
	mk := func() *Watcher {
		return &Watcher{Store: st, Config: status.DefaultConfig(), Now: func() time.Time { return now }, State: state,
			Send: func(string, string) error { sent++; return nil }}
	}
	mk().Check()
	if sent != 1 || !state["a"] {
		t.Fatalf("Alarm und Zustand: %d %v", sent, state)
	}
	mk().Check() // "Neustart": neuer Watcher liest den Zustand, kein erneuter Alarm
	if sent != 1 {
		t.Errorf("erneuter Alarm nach Neustart: %d", sent)
	}
}
