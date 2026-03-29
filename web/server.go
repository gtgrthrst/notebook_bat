package web

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	_ "embed"

	"notebook_bat/battery"
	"notebook_bat/config"
	"notebook_bat/logbuf"
	"notebook_bat/storage"
)

//go:embed static/index.html
var indexHTML string

// statusJSON is the live payload sent to the browser via SSE.
type statusJSON struct {
	Percent     int     `json:"percent"`
	ACOnline    bool    `json:"ac_online"`
	Charging    bool    `json:"charging"`
	TimeLeft    string  `json:"time_left"`
	DesignedCap int     `json:"designed_cap"`
	FullCap     int     `json:"full_cap"`
	Health      float64 `json:"health"`
	CycleCount  int     `json:"cycle_count"`
	VoltageMV   int     `json:"voltage_mv"`
	RateMW      int     `json:"rate_mw"`
	CapNowMWh   int     `json:"cap_now_mwh"`
	RateSource  string  `json:"rate_source"` // "ioctl" | "ntquery" | "estimated" | ""
}

func toJSON(info battery.Info, cap battery.CapacityInfo, rate battery.RateInfo) statusJSON {
	return statusJSON{
		Percent:     info.Percent,
		ACOnline:    info.ACStatus == battery.ACOnline,
		Charging:    info.Charging,
		TimeLeft:    info.TimeLeft(),
		DesignedCap: cap.DesignedCapacity,
		FullCap:     cap.FullChargedCapacity,
		Health:      cap.HealthPercent,
		CycleCount:  cap.CycleCount,
		VoltageMV:   rate.VoltageMV,
		RateMW:      rate.RateMW,
		CapNowMWh:   rate.CapacityMWh,
		RateSource:  rate.Source,
	}
}

// Server runs the HTTP dashboard and SSE stream.
type Server struct {
	cfg       *config.Config
	addr      string
	mu        sync.RWMutex
	latest    battery.Info
	latestCap battery.CapacityInfo
	latestRate battery.RateInfo
	clients   map[chan string]struct{}
	clientMu  sync.Mutex
	tmpl      *template.Template
	store     *storage.Store
	logBuf    *logbuf.Buffer
}

func New(addr string, cfg *config.Config) (*Server, error) {
	tmpl, err := template.New("index").Parse(indexHTML)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	return &Server{
		cfg:     cfg,
		addr:    addr,
		clients: make(map[chan string]struct{}),
		tmpl:    tmpl,
	}, nil
}

func (s *Server) SetStore(st *storage.Store)    { s.store = st }
func (s *Server) SetLogBuffer(b *logbuf.Buffer) { s.logBuf = b }

// Push is called by the monitor on every poll cycle.
func (s *Server) Push(info battery.Info, cap battery.CapacityInfo, rate battery.RateInfo) {
	s.mu.Lock()
	s.latest = info
	s.latestCap = cap
	s.latestRate = rate
	s.mu.Unlock()

	data, _ := json.Marshal(toJSON(info, cap, rate))
	msg := "data: " + string(data) + "\n\n"

	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Start listens and serves until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/stream", s.handleStream)
	// Report endpoints
	mux.HandleFunc("/api/report/summary", s.handleReportSummary)
	mux.HandleFunc("/api/report/daily", s.handleReportDaily)
	mux.HandleFunc("/api/report/capacity", s.handleReportCapacity)
	mux.HandleFunc("/api/report/power", s.handleReportPower)
	mux.HandleFunc("/api/report/processes", s.handleReportProcesses)
	mux.HandleFunc("/api/report/proctimeline", s.handleReportProcTimeline)
	mux.HandleFunc("/api/report/recent", s.handleReportRecent)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/export/dates", s.handleExportDates)
	mux.HandleFunc("/api/export/csv", s.handleExportCSV)

	srv := &http.Server{Addr: s.addr, Handler: mux}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	log.Printf("[web] dashboard → http://localhost%s", s.addr)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data := struct{ WarnLevel, CriticalLevel, FullLevel int }{
		s.cfg.WarnLevel, s.cfg.CriticalLevel, s.cfg.FullLevel,
	}
	var buf bytes.Buffer
	if err := s.tmpl.Execute(&buf, data); err != nil {
		http.Error(w, "template error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	info, cap, rate := s.latest, s.latestCap, s.latestRate
	s.mu.RUnlock()
	jsonOK(w, toJSON(info, cap, rate))
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan string, 4)
	s.clientMu.Lock()
	s.clients[ch] = struct{}{}
	s.clientMu.Unlock()
	defer func() {
		s.clientMu.Lock()
		delete(s.clients, ch)
		s.clientMu.Unlock()
	}()

	s.mu.RLock()
	if data, err := json.Marshal(toJSON(s.latest, s.latestCap, s.latestRate)); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	s.mu.RUnlock()

	for {
		select {
		case msg := <-ch:
			if _, err := fmt.Fprint(w, msg); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ── Report endpoints ──────────────────────────────────────────────────────────

func (s *Server) handleReportSummary(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		jsonError(w, "storage not enabled", 503)
		return
	}
	sum, err := s.store.GetSummary()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonOK(w, sum)
}

func (s *Server) handleReportDaily(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		jsonError(w, "storage not enabled", 503)
		return
	}
	stats, err := s.store.DailyStats(queryInt(r, "days", 30))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonOK(w, stats)
}

func (s *Server) handleReportCapacity(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		jsonError(w, "storage not enabled", 503)
		return
	}
	pts, err := s.store.CapacityHistory(queryInt(r, "days", 90))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonOK(w, pts)
}

func (s *Server) handleReportPower(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		jsonError(w, "storage not enabled", 503)
		return
	}
	pts, err := s.store.PowerHistory(queryInt(r, "hours", 24))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonOK(w, pts)
}

func (s *Server) handleReportProcesses(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		jsonError(w, "storage not enabled", 503)
		return
	}
	avgs, err := s.store.TopProcesses(
		queryInt(r, "hours", 24),
		queryInt(r, "limit", 20),
	)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonOK(w, avgs)
}

func (s *Server) handleReportProcTimeline(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		jsonError(w, "storage not enabled", 503)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		jsonError(w, "name required", 400)
		return
	}
	pts, err := s.store.ProcessTimeline(name, queryInt(r, "hours", 24))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonOK(w, pts)
}

func (s *Server) handleReportRecent(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		jsonError(w, "storage not enabled", 503)
		return
	}
	rows, err := s.store.RecentReadings(queryInt(r, "n", 60))
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonOK(w, rows)
}

func (s *Server) handleExportDates(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		jsonError(w, "storage not enabled", 503)
		return
	}
	dates, err := s.store.AvailableDates()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonOK(w, dates)
}

func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		jsonError(w, "storage not enabled", 503)
		return
	}
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	rows, err := s.store.DailyReadings(date)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	filename := "battery-" + date + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// UTF-8 BOM so Excel opens correctly
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"時間", "電量%", "插電", "充電中",
		"剩餘秒", "設計容量mWh", "實際容量mWh", "循環次數",
		"電壓mV", "功率mW", "目前容量mWh",
	})
	loc := time.Local
	for _, row := range rows {
		t := time.Unix(row.TS, 0).In(loc).Format("2006-01-02 15:04:05")
		ac := "否"
		if row.AC {
			ac = "是"
		}
		chg := "否"
		if row.Charging {
			chg = "是"
		}
		_ = cw.Write([]string{
			t,
			strconv.Itoa(row.Percent),
			ac, chg,
			strconv.Itoa(row.SecsLeft),
			strconv.Itoa(row.DesignedCap),
			strconv.Itoa(row.FullCap),
			strconv.Itoa(row.CycleCount),
			strconv.Itoa(row.VoltageMV),
			strconv.Itoa(row.RateMW),
			strconv.Itoa(row.CapNowMWh),
		})
	}
	cw.Flush()
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.logBuf == nil {
		jsonOK(w, []string{})
		return
	}
	jsonOK(w, s.logBuf.Lines())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
