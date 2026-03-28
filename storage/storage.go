// Package storage persists battery readings and process snapshots to SQLite.
package storage

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"notebook_bat/battery"
	"notebook_bat/sysmon"
)

// ── Schema ────────────────────────────────────────────────────────────────────

const baseSchema = `
CREATE TABLE IF NOT EXISTS readings (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           INTEGER NOT NULL,
    percent      INTEGER,
    ac           INTEGER,
    charging     INTEGER,
    secs_left    INTEGER DEFAULT -1,
    designed_cap INTEGER DEFAULT 0,
    full_cap     INTEGER DEFAULT 0,
    cycle_count  INTEGER DEFAULT 0,
    voltage_mv   INTEGER DEFAULT 0,
    rate_mw      INTEGER DEFAULT 0,
    cap_now_mwh  INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ts ON readings(ts);

CREATE TABLE IF NOT EXISTS proc_snapshots (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts      INTEGER NOT NULL,
    name    TEXT    NOT NULL,
    pid     INTEGER,
    cpu_pct REAL    DEFAULT 0,
    mem_mb  REAL    DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_proc_ts   ON proc_snapshots(ts);
CREATE INDEX IF NOT EXISTS idx_proc_name ON proc_snapshots(name, ts);
`

// columns added after initial release — applied via idempotent ALTER TABLE
var alterMigrations = []string{
	`ALTER TABLE readings ADD COLUMN voltage_mv  INTEGER DEFAULT 0`,
	`ALTER TABLE readings ADD COLUMN rate_mw     INTEGER DEFAULT 0`,
	`ALTER TABLE readings ADD COLUMN cap_now_mwh INTEGER DEFAULT 0`,
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path and applies schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite single-writer
	if _, err := db.Exec(baseSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	for _, m := range alterMigrations {
		_ = db // ignore "duplicate column" errors intentionally
		db.Exec(m)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() { _ = s.db.Close() }

// ── Write methods ─────────────────────────────────────────────────────────────

// Record inserts one battery reading.
func (s *Store) Record(info battery.Info, cap battery.CapacityInfo, rate battery.RateInfo) error {
	_, err := s.db.Exec(
		`INSERT INTO readings
		 (ts,percent,ac,charging,secs_left,designed_cap,full_cap,cycle_count,voltage_mv,rate_mw,cap_now_mwh)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		time.Now().Unix(),
		info.Percent,
		boolInt(info.ACStatus == battery.ACOnline),
		boolInt(info.Charging),
		info.SecsLeft,
		cap.DesignedCapacity,
		cap.FullChargedCapacity,
		cap.CycleCount,
		rate.VoltageMV,
		rate.RateMW,
		rate.CapacityMWh,
	)
	return err
}

// RecordProcesses inserts a batch of process snapshots.
func (s *Store) RecordProcesses(procs []sysmon.ProcessStat) error {
	if len(procs) == 0 {
		return nil
	}
	ts := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		`INSERT INTO proc_snapshots(ts,name,pid,cpu_pct,mem_mb) VALUES(?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, p := range procs {
		if _, err := stmt.Exec(ts, p.Name, p.PID, p.CPUPct, p.MemMB); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ── Report types ──────────────────────────────────────────────────────────────

type DailyStat struct {
	Date        string  `json:"date"`
	AvgPct      float64 `json:"avg_pct"`
	MinPct      int     `json:"min_pct"`
	MaxPct      int     `json:"max_pct"`
	Readings    int     `json:"readings"`
	FullCap     int     `json:"full_cap"`
	DesignedCap int     `json:"designed_cap"`
	Health      float64 `json:"health"`
}

type CapacityPoint struct {
	Date        string  `json:"date"`
	FullCap     int     `json:"full_cap"`
	DesignedCap int     `json:"designed_cap"`
	Health      float64 `json:"health"`
}

type PowerPoint struct {
	TS         int64   `json:"ts"`
	VoltageMV  int     `json:"voltage_mv"`
	RateMW     int     `json:"rate_mw"`
	CapNowMWh  int     `json:"cap_now_mwh"`
	Percent    int     `json:"percent"`
}

type ProcessAvg struct {
	Name    string  `json:"name"`
	AvgCPU  float64 `json:"avg_cpu"`
	MaxCPU  float64 `json:"max_cpu"`
	AvgMem  float64 `json:"avg_mem"`
	Samples int     `json:"samples"`
}

type ProcTimePoint struct {
	TS     int64   `json:"ts"`
	CPUPct float64 `json:"cpu_pct"`
	MemMB  float64 `json:"mem_mb"`
}

type Summary struct {
	TotalReadings int     `json:"total_readings"`
	FirstDate     string  `json:"first_date"`
	LastDate      string  `json:"last_date"`
	DesignedCap   int     `json:"designed_cap"`
	FullCap       int     `json:"full_cap"`
	CycleCount    int     `json:"cycle_count"`
	Health        float64 `json:"health"`
}

// ── Query methods ─────────────────────────────────────────────────────────────

func (s *Store) GetSummary() (Summary, error) {
	var sum Summary
	row := s.db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(date(MIN(ts),'unixepoch','localtime'),''),
			COALESCE(date(MAX(ts),'unixepoch','localtime'),''),
			COALESCE((SELECT designed_cap FROM readings WHERE designed_cap>0 ORDER BY ts DESC LIMIT 1),0),
			COALESCE((SELECT full_cap     FROM readings WHERE full_cap>0     ORDER BY ts DESC LIMIT 1),0),
			COALESCE((SELECT cycle_count  FROM readings WHERE cycle_count>0  ORDER BY ts DESC LIMIT 1),0)
		FROM readings
	`)
	if err := row.Scan(&sum.TotalReadings, &sum.FirstDate, &sum.LastDate,
		&sum.DesignedCap, &sum.FullCap, &sum.CycleCount); err != nil {
		return sum, err
	}
	if sum.DesignedCap > 0 {
		sum.Health = float64(sum.FullCap) / float64(sum.DesignedCap) * 100
	}
	return sum, nil
}

func (s *Store) DailyStats(days int) ([]DailyStat, error) {
	rows, err := s.db.Query(`
		WITH last_cap AS (
			SELECT
				date(ts,'unixepoch','localtime') AS day,
				full_cap, designed_cap,
				ROW_NUMBER() OVER (
					PARTITION BY date(ts,'unixepoch','localtime') ORDER BY ts DESC
				) AS rn
			FROM readings
			WHERE ts >= strftime('%s','now') - ? * 86400
		)
		SELECT
			a.day,
			ROUND(AVG(r.percent),1),
			MIN(r.percent),
			MAX(r.percent),
			COUNT(r.id),
			COALESCE(c.full_cap,0),
			COALESCE(c.designed_cap,0)
		FROM readings r
		JOIN (
			SELECT DISTINCT date(ts,'unixepoch','localtime') AS day
			FROM readings WHERE ts >= strftime('%s','now') - ? * 86400
		) a ON date(r.ts,'unixepoch','localtime') = a.day
		LEFT JOIN last_cap c ON c.day = a.day AND c.rn = 1
		GROUP BY a.day
		ORDER BY a.day DESC
	`, days, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []DailyStat
	for rows.Next() {
		var d DailyStat
		if err := rows.Scan(&d.Date, &d.AvgPct, &d.MinPct, &d.MaxPct,
			&d.Readings, &d.FullCap, &d.DesignedCap); err != nil {
			continue
		}
		if d.DesignedCap > 0 {
			d.Health = float64(d.FullCap) / float64(d.DesignedCap) * 100
		}
		stats = append(stats, d)
	}
	return stats, rows.Err()
}

func (s *Store) CapacityHistory(days int) ([]CapacityPoint, error) {
	rows, err := s.db.Query(`
		SELECT date(ts,'unixepoch','localtime'), full_cap, designed_cap
		FROM readings
		WHERE designed_cap > 0
		  AND ts IN (
			SELECT MAX(ts) FROM readings
			WHERE designed_cap > 0
			  AND ts >= strftime('%s','now') - ? * 86400
			GROUP BY date(ts,'unixepoch','localtime')
		  )
		ORDER BY ts ASC
	`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pts []CapacityPoint
	for rows.Next() {
		var p CapacityPoint
		if err := rows.Scan(&p.Date, &p.FullCap, &p.DesignedCap); err != nil {
			continue
		}
		if p.DesignedCap > 0 {
			p.Health = float64(p.FullCap) / float64(p.DesignedCap) * 100
		}
		pts = append(pts, p)
	}
	return pts, rows.Err()
}

// PowerHistory returns voltage, rate and capacity readings for the last `hours`
// hours, sampled (max 500 points).
func (s *Store) PowerHistory(hours int) ([]PowerPoint, error) {
	// Downsample: calculate stride so at most 500 rows are returned.
	total := hours * 60 // rough estimate assuming 1 reading/min
	stride := total / 500
	if stride < 1 {
		stride = 1
	}
	rows, err := s.db.Query(`
		SELECT ts, voltage_mv, rate_mw, cap_now_mwh, percent
		FROM (
			SELECT ts, voltage_mv, rate_mw, cap_now_mwh, percent,
			       ROW_NUMBER() OVER (ORDER BY ts) AS rn
			FROM readings
			WHERE ts >= strftime('%s','now') - ? * 3600
			  AND (voltage_mv > 0 OR rate_mw != 0)
		)
		WHERE rn % ? = 1
		ORDER BY ts ASC
	`, hours, stride)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pts []PowerPoint
	for rows.Next() {
		var p PowerPoint
		if err := rows.Scan(&p.TS, &p.VoltageMV, &p.RateMW, &p.CapNowMWh, &p.Percent); err != nil {
			continue
		}
		pts = append(pts, p)
	}
	return pts, rows.Err()
}

// TopProcesses returns the top `limit` processes by average CPU% over the last
// `hours` hours.
func (s *Store) TopProcesses(hours, limit int) ([]ProcessAvg, error) {
	rows, err := s.db.Query(`
		SELECT name, ROUND(AVG(cpu_pct),2), ROUND(MAX(cpu_pct),2),
		       ROUND(AVG(mem_mb),1), COUNT(*)
		FROM proc_snapshots
		WHERE ts >= strftime('%s','now') - ? * 3600
		GROUP BY name
		ORDER BY AVG(cpu_pct) DESC
		LIMIT ?
	`, hours, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var avgs []ProcessAvg
	for rows.Next() {
		var a ProcessAvg
		if err := rows.Scan(&a.Name, &a.AvgCPU, &a.MaxCPU, &a.AvgMem, &a.Samples); err != nil {
			continue
		}
		avgs = append(avgs, a)
	}
	return avgs, rows.Err()
}

// ProcessTimeline returns hourly CPU% samples for a named process.
func (s *Store) ProcessTimeline(name string, hours int) ([]ProcTimePoint, error) {
	rows, err := s.db.Query(`
		SELECT ts, cpu_pct, mem_mb
		FROM proc_snapshots
		WHERE name = ?
		  AND ts >= strftime('%s','now') - ? * 3600
		ORDER BY ts ASC
	`, name, hours)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pts []ProcTimePoint
	for rows.Next() {
		var p ProcTimePoint
		if err := rows.Scan(&p.TS, &p.CPUPct, &p.MemMB); err != nil {
			continue
		}
		pts = append(pts, p)
	}
	return pts, rows.Err()
}

// RecentReadings returns the last n readings for sparkline seeding.
type RawReading struct {
	TS      int64 `json:"ts"`
	Percent int   `json:"percent"`
	AC      bool  `json:"ac"`
	Charging bool `json:"charging"`
}

func (s *Store) RecentReadings(n int) ([]RawReading, error) {
	rows, err := s.db.Query(`
		SELECT ts, percent, ac, charging FROM readings ORDER BY ts DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawReading
	for rows.Next() {
		var r RawReading
		var ac, charging int
		if err := rows.Scan(&r.TS, &r.Percent, &ac, &charging); err != nil {
			continue
		}
		r.AC, r.Charging = ac == 1, charging == 1
		out = append(out, r)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
