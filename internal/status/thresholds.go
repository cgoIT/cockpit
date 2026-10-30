package status

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// thresholdFile ist die optionale Schwellenwert-Datei von Sitewatch. Fehlende Felder behalten den Standardwert.
// Einheiten stehen im Namen, damit es keine Parsing-Fallen gibt (time.ParseDuration kennt keine Tage).
type thresholdFile struct {
	StaleAfterMinutes *int `json:"stale_after_minutes"`
	ErrorWarn         *int `json:"error_warn"`
	EOLWarnDays       *int `json:"eol_warn_days"`
	TLSWarnDays       *int `json:"tls_warn_days"`
	BackupMaxHours    *int `json:"backup_max_hours"`
	DiskWarnPct       *int `json:"disk_warn_pct"`
	DiskCritPct       *int `json:"disk_crit_pct"`
	CronWarnMinutes   *int `json:"cron_warn_minutes"`
	CronCritMinutes   *int `json:"cron_crit_minutes"`
	MaintWarnMinutes  *int `json:"maintenance_warn_minutes"`
	MaintCritMinutes  *int `json:"maintenance_crit_minutes"`
	HistoryFullDays   *int `json:"history_full_days"`
	HistorySixHDays   *int `json:"history_6h_days"`
	HistoryMaxDays    *int `json:"history_max_days"`
	HistoryReportDays *int `json:"history_reports_days"`
}

// LoadThresholds überlagert cfg mit den Werten aus der JSON-Datei. Leerer Pfad = cfg unverändert.
func LoadThresholds(path string, cfg Config) (Config, error) {
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	var f thresholdFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // Tippfehler nicht stillschweigend ignorieren
	if err := dec.Decode(&f); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	for name, v := range map[string]struct {
		val *int
		set func(int)
	}{
		"stale_after_minutes":      {f.StaleAfterMinutes, func(n int) { cfg.StaleAfter = time.Duration(n) * time.Minute }},
		"error_warn":               {f.ErrorWarn, func(n int) { cfg.ErrorWarn = n }},
		"eol_warn_days":            {f.EOLWarnDays, func(n int) { cfg.EOLWarn = time.Duration(n) * 24 * time.Hour }},
		"tls_warn_days":            {f.TLSWarnDays, func(n int) { cfg.TLSWarn = time.Duration(n) * 24 * time.Hour }},
		"backup_max_hours":         {f.BackupMaxHours, func(n int) { cfg.BackupMax = time.Duration(n) * time.Hour }},
		"disk_warn_pct":            {f.DiskWarnPct, func(n int) { cfg.DiskWarn = n }},
		"cron_warn_minutes":        {f.CronWarnMinutes, func(n int) { cfg.CronWarn = time.Duration(n) * time.Minute }},
		"history_full_days":        {f.HistoryFullDays, func(n int) { cfg.Retention.FullDays = n }},
		"history_6h_days":          {f.HistorySixHDays, func(n int) { cfg.Retention.SixHourDays = n }},
		"history_max_days":         {f.HistoryMaxDays, func(n int) { cfg.Retention.MaxDays = n }},
		"history_reports_days":     {f.HistoryReportDays, func(n int) { cfg.Retention.ReportDays = n }},
		"maintenance_warn_minutes": {f.MaintWarnMinutes, func(n int) { cfg.MaintWarn = time.Duration(n) * time.Minute }},
		"maintenance_crit_minutes": {f.MaintCritMinutes, func(n int) { cfg.MaintCrit = time.Duration(n) * time.Minute }},
		"cron_crit_minutes":        {f.CronCritMinutes, func(n int) { cfg.CronCrit = time.Duration(n) * time.Minute }},
		"disk_crit_pct":            {f.DiskCritPct, func(n int) { cfg.DiskCrit = n }},
	} {
		if v.val == nil {
			continue
		}
		if *v.val < 1 {
			return cfg, fmt.Errorf("%s: %s muss mindestens 1 sein", path, name)
		}
		v.set(*v.val)
	}
	if cfg.DiskWarn > 100 || cfg.DiskCrit > cfg.DiskWarn {
		return cfg, errors.New(path + ": disk_crit_pct darf nicht größer als disk_warn_pct sein, disk_warn_pct höchstens 100")
	}
	if cfg.MaintWarn > cfg.MaintCrit {
		return cfg, errors.New(path + ": maintenance_warn_minutes darf nicht größer als maintenance_crit_minutes sein")
	}
	if cfg.CronWarn > cfg.CronCrit {
		return cfg, errors.New(path + ": cron_warn_minutes darf nicht größer als cron_crit_minutes sein")
	}
	if r := cfg.Retention; r.FullDays > r.SixHourDays || r.SixHourDays > r.MaxDays {
		return cfg, errors.New(path + ": es muss gelten history_full_days <= history_6h_days <= history_max_days")
	}
	return cfg, nil
}
