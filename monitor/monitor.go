package monitor

import (
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"notebook_bat/battery"
	"notebook_bat/config"
	"notebook_bat/notifier"
	"notebook_bat/storage"
	"notebook_bat/sysmon"
)

type state struct {
	lastPercent  int
	lastACStatus battery.ACStatus
	warnSent     bool
	criticalSent bool
	fullSent     bool

	// For delta-based rate estimation when hardware API returns unknown rate.
	lastCapMWh  int
	lastCapTime time.Time
}

// Pusher is implemented by the web server to receive live battery updates.
type Pusher interface {
	Push(battery.Info, battery.CapacityInfo, battery.RateInfo)
}

type Monitor struct {
	cfg    *config.Config
	logger *log.Logger
	state  state
	pusher Pusher
	store  *storage.Store
}

func New(cfg *config.Config) *Monitor {
	var w io.Writer = os.Stdout
	if cfg.LogToFile {
		if f, err := os.OpenFile("battery.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			w = io.MultiWriter(os.Stdout, f)
		}
	}
	return &Monitor{
		cfg:    cfg,
		logger: log.New(w, "", log.LstdFlags),
		state:  state{lastPercent: -1},
	}
}

func (m *Monitor) SetPusher(p Pusher)        { m.pusher = p }
func (m *Monitor) SetStore(s *storage.Store) { m.store = s }

// Run starts the monitoring loop. Blocks until stop is closed.
func (m *Monitor) Run(stop <-chan struct{}) {
	m.logger.Println("[battery-monitor] started")

	// Prime the process CPU sampler (first call establishes baseline).
	sysmon.TopProcesses(15)

	var lastInfo battery.Info
	for {
		info, err := battery.Get()
		if err != nil {
			m.logger.Printf("[ERROR] battery.Get: %v", err)
		} else {
			lastInfo = info
			m.process(info)
		}

		interval := m.nextInterval(lastInfo)
		select {
		case <-stop:
			m.logger.Println("[battery-monitor] stopped")
			return
		case <-time.After(interval):
		}
	}
}

func (m *Monitor) nextInterval(info battery.Info) time.Duration {
	if info.Percent >= 0 && info.Percent <= m.cfg.CriticalLevel {
		return m.cfg.CriticalInterval.Duration
	}
	if info.Percent >= 0 && info.Percent <= m.cfg.WarnLevel {
		return m.cfg.WarnInterval.Duration
	}
	return m.cfg.NormalInterval.Duration
}

func (m *Monitor) process(info battery.Info) {
	if info.NoBattery {
		return
	}

	// ── Capacity (cached 10 min) ───────────────────────────────────────────
	cap, err := battery.GetCapacity()
	if err != nil {
		m.logger.Printf("[WARN] GetCapacity: %v", err)
	}

	// ── Real-time rate / voltage ───────────────────────────────────────────
	rate, err := battery.GetRateInfo()
	if err != nil {
		m.logger.Printf("[WARN] GetRateInfo: %v", err)
	}

	// ── Delta-based rate estimation (fallback when hardware returns 0) ─────
	// Uses consecutive CapacityMWh readings to compute ΔmWh/Δt in mW.
	now := time.Now()
	if rate.CapacityMWh > 0 {
		if m.state.lastCapMWh > 0 && now.After(m.state.lastCapTime) {
			elapsed := now.Sub(m.state.lastCapTime).Hours()
			if elapsed > 0 && rate.RateMW == 0 {
				deltaWh := float64(rate.CapacityMWh-m.state.lastCapMWh) / 1000.0
				rate.RateMW = int(deltaWh / elapsed * 1000) // back to mW
				rate.Source = "estimated"
			}
		}
		m.state.lastCapMWh = rate.CapacityMWh
		m.state.lastCapTime = now
	} else if cap.FullChargedCapacity > 0 && info.Percent >= 0 {
		// No mWh from hardware — estimate from percent × full capacity.
		estMWh := cap.FullChargedCapacity * info.Percent / 100
		if m.state.lastCapMWh > 0 && now.After(m.state.lastCapTime) {
			elapsed := now.Sub(m.state.lastCapTime).Hours()
			if elapsed > 0 && rate.RateMW == 0 {
				deltaWh := float64(estMWh-m.state.lastCapMWh) / 1000.0
				rate.RateMW = int(deltaWh / elapsed * 1000)
				rate.Source = "estimated"
				rate.CapacityMWh = estMWh
			}
		}
		m.state.lastCapMWh = estMWh
		m.state.lastCapTime = now
	}

	// ── Log ───────────────────────────────────────────────────────────────
	if cap.DesignedCapacity > 0 {
		m.logger.Printf("[info] %s | health=%.1f%% | %dmV | %+dmW",
			info, cap.HealthPercent, rate.VoltageMV, rate.RateMW)
	} else {
		m.logger.Printf("[info] %s | %dmV | %+dmW",
			info, rate.VoltageMV, rate.RateMW)
	}

	// ── Web push ──────────────────────────────────────────────────────────
	if m.pusher != nil {
		m.pusher.Push(info, cap, rate)
	}

	// ── Persist battery reading ───────────────────────────────────────────
	if m.store != nil {
		if err := m.store.Record(info, cap, rate); err != nil {
			m.logger.Printf("[WARN] store.Record: %v", err)
		}
	}

	// ── Process snapshot (top 15 by CPU) ──────────────────────────────────
	if m.store != nil {
		procs := sysmon.TopProcesses(15)
		if err := m.store.RecordProcesses(procs); err != nil {
			m.logger.Printf("[WARN] store.RecordProcesses: %v", err)
		}
	}

	pct := info.Percent

	// ── AC plug/unplug detection ──────────────────────────────────────────
	if m.state.lastPercent >= 0 && info.ACStatus != m.state.lastACStatus {
		if info.ACStatus == battery.ACOffline && m.cfg.NotifyOnUnplug {
			notifier.Unplugged(pct)
			m.logAlert(fmt.Sprintf("AC unplugged at %d%%", pct))
		} else if info.ACStatus == battery.ACOnline && m.cfg.NotifyOnPlug {
			notifier.PluggedIn(pct)
			m.logAlert(fmt.Sprintf("AC plugged in at %d%%", pct))
		}
		m.state.warnSent = false
		m.state.criticalSent = false
		m.state.fullSent = false
	}

	// ── Battery level alerts (discharging only) ───────────────────────────
	if info.ACStatus == battery.ACOffline && pct >= 0 {
		if pct <= m.cfg.CriticalLevel {
			if !m.cfg.SuppressRepeats || !m.state.criticalSent {
				notifier.CriticalBattery(pct)
				m.logAlert(fmt.Sprintf("CRITICAL: battery at %d%%", pct))
				m.state.criticalSent = true
				m.state.warnSent = true
			}
		} else if pct <= m.cfg.WarnLevel {
			if !m.cfg.SuppressRepeats || !m.state.warnSent {
				notifier.LowBattery(pct)
				m.logAlert(fmt.Sprintf("WARNING: battery at %d%%", pct))
				m.state.warnSent = true
			}
		} else {
			m.state.warnSent = false
			m.state.criticalSent = false
		}
	}

	// ── Full charge alert ─────────────────────────────────────────────────
	if info.ACStatus == battery.ACOnline && pct >= m.cfg.FullLevel && m.cfg.NotifyOnFull {
		if !m.cfg.SuppressRepeats || !m.state.fullSent {
			notifier.FullyCharged(pct)
			m.logAlert(fmt.Sprintf("battery full at %d%%", pct))
			m.state.fullSent = true
		}
	} else if pct < m.cfg.FullLevel {
		m.state.fullSent = false
	}

	m.state.lastPercent = pct
	m.state.lastACStatus = info.ACStatus
}

func (m *Monitor) logAlert(msg string) {
	m.logger.Printf("[ALERT] %s", msg)
}
