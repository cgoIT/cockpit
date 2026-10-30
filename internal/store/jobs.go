package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Status eines Auftrags. "abgelaufen" ist kein gespeicherter Zustand: wartend und nach expires_ns.
const (
	JobQueued   = "queued"   // freigegeben, wartet auf den Agenten
	JobRunning  = "running"  // der Agent hat bestätigt und führt aus
	JobOK       = "ok"       // Exit 0
	JobFailed   = "failed"   // Exit ungleich 0, Zeitlimit oder Startfehler
	JobCanceled = "canceled" // vom Benutzer abgebrochen
	JobRejected = "rejected" // der Agent hat den Auftrag abgelehnt (Signatur, nicht erlaubt, schon gesehen, ...)
)

// MaxJobOutput begrenzt die gespeicherte Ausgabe je Auftrag; darüber hinaus wird nichts mehr gespeichert (Truncated).
const MaxJobOutput = 256 << 10

// jobStale: ein "laufender" Auftrag, dessen Start so lange her ist, blockiert die Site nicht mehr (Agent abgestürzt).
const jobStale = 45 * time.Minute

// redeliverWindow: so lange nach "running" wird ein Auftrag ohne Ausgabe noch einmal ausgeliefert.
const redeliverWindow = 2 * time.Minute

// ErrJobBusy: für die Site wartet oder läuft schon ein Auftrag.
var ErrJobBusy = errors.New("für diese Site wartet oder läuft schon ein Auftrag")

// Job ist ein Auftrag mit Zustand.
type Job struct {
	ID, Agent, Site, Action string
	Status, Message         string
	ApprovedBy              string
	CreatedAt, ExpiresAt    time.Time
	StartedAt, FinishedAt   time.Time // Nullwert = noch nicht
	ExitCode                *int
	Envelope                []byte // signierter Auftrag, genau so geht er an den Agenten
	CancelRequested         bool
	OutBytes                int
	Truncated               bool
}

// Active: der Auftrag ist noch nicht beendet. Abgelaufene wartende und "laufende", deren Agent sich seit jobStale nicht
// mehr gemeldet haben kann (abgestürzt), zählen nicht mehr.
func (j Job) Active(now time.Time) bool {
	return j.Status == JobRunning && now.Sub(j.StartedAt) < jobStale || j.Status == JobQueued && now.Before(j.ExpiresAt)
}

// Hung: als "läuft" gespeichert, aber der Start liegt länger als jobStale zurück.
func (j Job) Hung(now time.Time) bool {
	return j.Status == JobRunning && now.Sub(j.StartedAt) >= jobStale
}

// Chunk ist ein Stück Ausgabe; Seq zählt der Agent ab 1, so ist Nachsenden harmlos.
type Chunk struct {
	Seq  int    `json:"seq"`
	Data string `json:"data"`
}

// Jobs ist die Schnittstelle zum Speicher der Aufträge.
type Jobs interface {
	CreateJob(j Job) error                                  // ErrJobBusy, wenn die Site schon einen aktiven Auftrag hat
	GetJob(id string) (Job, bool, error)                    // ohne Ausgabe
	ListJobs(limit int) ([]Job, error)                      // neueste zuerst
	NextJob(agent string, now time.Time) (Job, bool, error) // ältester wartender, nicht abgelaufener Auftrag
	StartJob(id string, now time.Time) (bool, error)        // wartend -> läuft; false = nicht starten (abgebrochen, abgelaufen, fremd)
	AppendChunks(id string, c []Chunk) (cancel bool, err error)
	FinishJob(id, status, message string, exit *int, now time.Time) error // nur aus wartend/läuft; sonst ohne Wirkung
	CancelJob(id string, now time.Time) error                             // wartend: sofort beendet; läuft: Abbruch angefordert
	Chunks(id string, after int) ([]Chunk, error)
}

const jobCols = `id, agent, site, action, status, message, approved_by, created_ns, expires_ns, started_ns, finished_ns, exit_code, envelope, cancel, out_bytes, truncated`

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var created, expires int64
	var started, finished, exit sql.NullInt64
	var env string
	var cancel, trunc int
	if err := sc.Scan(&j.ID, &j.Agent, &j.Site, &j.Action, &j.Status, &j.Message, &j.ApprovedBy, &created, &expires, &started, &finished, &exit, &env, &cancel, &j.OutBytes, &trunc); err != nil {
		return Job{}, err
	}
	j.CreatedAt, j.ExpiresAt = time.Unix(0, created), time.Unix(0, expires)
	if started.Valid {
		j.StartedAt = time.Unix(0, started.Int64)
	}
	if finished.Valid {
		j.FinishedAt = time.Unix(0, finished.Int64)
	}
	j.ExitCode = ptr(exit)
	j.Envelope, j.CancelRequested, j.Truncated = []byte(env), cancel == 1, trunc == 1
	return j, nil
}

func (s *SQLite) CreateJob(j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE site=? AND ((status='queued' AND expires_ns>?) OR (status='running' AND started_ns>?))`,
		j.Site, j.CreatedAt.UnixNano(), j.CreatedAt.Add(-jobStale).UnixNano()).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrJobBusy
	}
	_, err := s.db.Exec(`INSERT INTO jobs(id, agent, site, action, status, approved_by, created_ns, expires_ns, envelope) VALUES(?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Agent, j.Site, j.Action, JobQueued, j.ApprovedBy, j.CreatedAt.UnixNano(), j.ExpiresAt.UnixNano(), string(j.Envelope))
	return err
}

func (s *SQLite) GetJob(id string) (Job, bool, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	return j, err == nil, err
}

func (s *SQLite) ListJobs(limit int) ([]Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs ORDER BY created_ns DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *SQLite) NextJob(agent string, now time.Time) (Job, bool, error) {
	// Ein "laufender" Auftrag ohne Ausgabe, dessen Start kürzlich war, kommt noch einmal: Die Antwort auf "running" kann verloren gegangen
	// sein, dann hat der Agent nichts ausgeführt. Hat er ihn doch gestartet, lehnt er die zweite Lieferung ab (ID schon gesehen) und der Auftrag endet.
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE agent=? AND cancel=0 AND
		((status='queued' AND expires_ns>?) OR (status='running' AND out_bytes=0 AND started_ns>?)) ORDER BY created_ns LIMIT 1`,
		agent, now.UnixNano(), now.Add(-redeliverWindow).UnixNano()))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	return j, err == nil, err
}

func (s *SQLite) StartJob(id string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE jobs SET status='running', started_ns=? WHERE id=? AND status='queued' AND cancel=0 AND expires_ns>?`, now.UnixNano(), id, now.UnixNano())
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	// Wiederholung nach verlorener Antwort: läuft schon und ist nicht zum Abbruch markiert.
	var n int
	err = s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE id=? AND status='running' AND cancel=0`, id).Scan(&n)
	return n == 1, err
}

func (s *SQLite) AppendChunks(id string, chunks []Chunk) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var status string
	var cancel, trunc, size int
	if err := tx.QueryRow(`SELECT status, cancel, truncated, out_bytes FROM jobs WHERE id=?`, id).Scan(&status, &cancel, &trunc, &size); err != nil {
		return false, err
	}
	if status != JobRunning { // beendet oder nie gestartet: der Agent soll aufhören
		return true, nil
	}
	for _, c := range chunks {
		if c.Seq < 1 {
			continue
		}
		if trunc == 1 || size+len(c.Data) > MaxJobOutput {
			trunc = 1 // ab hier nichts mehr, sonst entstünden Lücken in der Ausgabe
			continue
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO job_chunks(job_id, seq, data) VALUES(?,?,?)`, id, c.Seq, c.Data)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			size += len(c.Data)
		}
	}
	if _, err := tx.Exec(`UPDATE jobs SET out_bytes=?, truncated=? WHERE id=?`, size, trunc, id); err != nil {
		return false, err
	}
	return cancel == 1, tx.Commit()
}

func (s *SQLite) FinishJob(id, status, message string, exit *int, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE jobs SET status=?, message=?, exit_code=?, finished_ns=? WHERE id=? AND status IN ('queued','running')`,
		status, message, nullInt(exit), now.UnixNano(), id)
	return err
}

func (s *SQLite) CancelJob(id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE jobs SET status='canceled', message='vor dem Start abgebrochen', finished_ns=? WHERE id=? AND status='queued'`, now.UnixNano(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	// Läuft er seit über jobStale (Agent abgestürzt), gibt es niemanden mehr, der den Abbruch erfährt: gleich beenden.
	res, err = s.db.Exec(`UPDATE jobs SET status='failed', message='Agent hat sich nicht mehr gemeldet', finished_ns=? WHERE id=? AND status='running' AND started_ns<=?`,
		now.UnixNano(), id, now.Add(-jobStale).UnixNano())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	_, err = s.db.Exec(`UPDATE jobs SET cancel=1 WHERE id=? AND status='running'`, id)
	return err
}

func (s *SQLite) Chunks(id string, after int) ([]Chunk, error) {
	rows, err := s.db.Query(`SELECT seq, data FROM job_chunks WHERE job_id=? AND seq>? ORDER BY seq`, id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.Seq, &c.Data); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Text fügt die Stücke zusammen.
func Text(chunks []Chunk) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Data)
	}
	return b.String()
}
