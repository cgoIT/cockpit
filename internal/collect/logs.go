package collect

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"cockpit/internal/report"
)

// Monolog-Zeile: [2026-10-27T19:02:11.123456+01:00] request.CRITICAL: Nachricht {...} []
var lineRe = regexp.MustCompile(`^\[([^\]]+)\] [^.\s]+\.([A-Z]+): (.*)$`)

var errorLevels = map[string]bool{"ERROR": true, "CRITICAL": true, "ALERT": true, "EMERGENCY": true}

const tailBytes = 512 * 1024

// ReadLogErrors liest Fehler (ERROR und höher) der letzten window aus var/logs/*.log
// und gruppiert gleiche Meldungen. Neueste zuerst.
func ReadLogErrors(dir string, now time.Time, window time.Duration, max int) ([]report.LogError, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		return []report.LogError{}, err
	}
	since := now.Add(-window)
	type key struct{ level, msg string }
	groups := map[key]*report.LogError{}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || st.ModTime().Before(since) {
			continue
		}
		lines, err := tailLines(f, tailBytes)
		if err != nil {
			continue
		}
		for _, line := range lines {
			m := lineRe.FindStringSubmatch(line)
			if m == nil || !errorLevels[m[2]] {
				continue
			}
			ts, err := time.Parse(time.RFC3339Nano, m[1])
			if err != nil || ts.Before(since) || ts.After(now.Add(5*time.Minute)) {
				continue
			}
			k := key{m[2], cleanMessage(m[3])}
			if g, ok := groups[k]; ok {
				g.Count++
				if ts.After(g.Time) {
					g.Time = ts
				}
			} else {
				groups[k] = &report.LogError{Time: ts.UTC(), Level: k.level, Message: k.msg, Count: 1}
			}
		}
	}
	out := make([]report.LogError, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// cleanMessage schneidet Monolog-Kontext (" {...}" / " []") ab und kürzt.
func cleanMessage(s string) string {
	for _, sep := range []string{" {\"", " []", " {}"} {
		if i := strings.Index(s, sep); i >= 0 {
			s = s[:i]
		}
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

func tailLines(path string, max int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	partial := false
	if st.Size() > max {
		if _, err := f.Seek(st.Size()-max, io.SeekStart); err != nil {
			return nil, err
		}
		partial = true
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if partial && len(lines) > 0 {
		lines = lines[1:] // erste Zeile ist angeschnitten
	}
	return lines, sc.Err()
}
