package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJobs(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "c.db"), DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	mk := func(id, site string, at time.Time) Job {
		return Job{ID: id, Agent: "a", Site: site, Action: "maintenance-on", ApprovedBy: "me", CreatedAt: at, ExpiresAt: at.Add(10 * time.Minute), Envelope: []byte(`{}`)}
	}
	if err := st.CreateJob(mk("j1", "s", now)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(mk("j2", "s", now)); !errors.Is(err, ErrJobBusy) {
		t.Fatalf("zweiter Auftrag für dieselbe Site: %v", err)
	}
	if err := st.CreateJob(mk("j3", "t", now)); err != nil {
		t.Fatal(err)
	}
	if j, ok, _ := st.NextJob("a", now); !ok || j.ID != "j1" {
		t.Fatalf("NextJob: %+v %v", j, ok)
	}
	if _, ok, _ := st.NextJob("b", now); ok {
		t.Error("fremder Agent bekommt nichts")
	}
	if _, ok, _ := st.NextJob("a", now.Add(11*time.Minute)); ok {
		t.Error("abgelaufene Aufträge werden nicht ausgeliefert")
	}
	if ok, _ := st.StartJob("j1", now.Add(11*time.Minute)); ok {
		t.Error("abgelaufen darf nicht starten")
	}

	// Abbruch vor dem Start: der Agent darf nicht mehr starten.
	if err := st.CancelJob("j3", now); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.StartJob("j3", now); ok {
		t.Error("abgebrochener Auftrag darf nicht starten")
	}

	if ok, err := st.StartJob("j1", now); !ok || err != nil {
		t.Fatalf("Start: %v %v", ok, err)
	}
	if ok, _ := st.StartJob("j1", now); !ok { // Wiederholung nach verlorener Antwort
		t.Error("zweiter Start-Aufruf eines laufenden Auftrags muss gehen")
	}
	if c, err := st.AppendChunks("j1", []Chunk{{1, "a\n"}, {2, "b\n"}}); c || err != nil {
		t.Fatalf("%v %v", c, err)
	}
	st.AppendChunks("j1", []Chunk{{2, "b\n"}, {3, "c\n"}}) // Nachsenden: 2 nur einmal
	if got, _ := st.Chunks("j1", 0); Text(got) != "a\nb\nc\n" {
		t.Errorf("Ausgabe %q", Text(got))
	}
	if got, _ := st.Chunks("j1", 2); len(got) != 1 || got[0].Seq != 3 {
		t.Errorf("ab Seq 2: %+v", got)
	}
	if err := st.CancelJob("j1", now); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.AppendChunks("j1", nil); !c {
		t.Error("angeforderter Abbruch muss zurückgemeldet werden")
	}
	// Obergrenze der Ausgabe
	big := strings.Repeat("x", MaxJobOutput)
	st.AppendChunks("j1", []Chunk{{4, big}})
	j, _, _ := st.GetJob("j1")
	if !j.Truncated || j.OutBytes > MaxJobOutput {
		t.Errorf("Obergrenze: %+v", j)
	}
	exit := 1
	if err := st.FinishJob("j1", JobCanceled, "abgebrochen", &exit, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	st.FinishJob("j1", JobOK, "", nil, now.Add(2*time.Minute)) // beendet bleibt beendet
	if j, _, _ := st.GetJob("j1"); j.Status != JobCanceled || j.ExitCode == nil || *j.ExitCode != 1 {
		t.Errorf("%+v", j)
	}
	if c, _ := st.AppendChunks("j1", nil); !c {
		t.Error("nach dem Ende soll der Agent aufhören")
	}
	// Die Site ist wieder frei.
	if err := st.CreateJob(mk("j4", "s", now.Add(3*time.Minute))); err != nil {
		t.Errorf("nach dem Ende wieder erlaubt: %v", err)
	}
	if l, _ := st.ListJobs(10); len(l) != 3 || l[0].ID != "j4" {
		t.Errorf("Liste: %+v", l)
	}
	// Aufräumen nach ReportDays
	if err := st.Maintain(now.AddDate(0, 0, 100)); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.ListJobs(10); len(l) != 0 {
		t.Errorf("alte Aufträge müssen weg: %+v", l)
	}
	if got, _ := st.Chunks("j1", 0); len(got) != 0 {
		t.Error("Ausgabe muss mit weg")
	}
}

func TestJobRedeliveryAndHung(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "c.db"), DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	j := Job{ID: "j1", Agent: "a", Site: "s", Action: "maintenance-on", ApprovedBy: "me", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), Envelope: []byte(`{}`)}
	if err := st.CreateJob(j); err != nil {
		t.Fatal(err)
	}
	st.StartJob("j1", now) // die Antwort darauf ging verloren: der Agent hat nichts ausgeführt
	if got, ok, _ := st.NextJob("a", now.Add(30*time.Second)); !ok || got.ID != "j1" {
		t.Error("laufender Auftrag ohne Ausgabe muss noch einmal ausgeliefert werden")
	}
	if _, ok, _ := st.NextJob("a", now.Add(3*time.Minute)); ok {
		t.Error("nach dem Zeitfenster nicht mehr")
	}
	// Mit Ausgabe wird nie erneut ausgeliefert.
	st.AppendChunks("j1", []Chunk{{1, "x"}})
	if _, ok, _ := st.NextJob("a", now.Add(30*time.Second)); ok {
		t.Error("ein Auftrag mit Ausgabe lief schon")
	}
	// Abgestürzter Agent: nach jobStale ist er beendet, "Abbrechen" macht daraus fehlgeschlagen.
	late := now.Add(jobStale + time.Minute)
	got, _, _ := st.GetJob("j1")
	if got.Active(now.Add(time.Minute)) != true || got.Active(late) || !got.Hung(late) {
		t.Errorf("Active/Hung: %+v", got)
	}
	if err := st.CancelJob("j1", late); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetJob("j1"); got.Status != JobFailed || got.Message == "" {
		t.Errorf("hängender Auftrag muss beendet werden: %+v", got)
	}
}
