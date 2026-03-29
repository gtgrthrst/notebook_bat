package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kardianos/service"
	"notebook_bat/battery"
	"notebook_bat/config"
	"notebook_bat/logbuf"
	"notebook_bat/monitor"
	"notebook_bat/storage"
	"notebook_bat/web"
)

// ── Flags ─────────────────────────────────────────────────────────────────────

var (
	cfgPath    = flag.String("config", "config.json", "path to config file")
	statusOnly = flag.Bool("status", false, "print current battery status and exit")
	genConfig  = flag.Bool("init", false, "write default config.json and exit")
	webAddr    = flag.String("web", ":8089", "web dashboard address (empty = disabled)")
	dbPath     = flag.String("db", "battery.db", "SQLite database path (empty = disabled)")
	svcCtrl    = flag.String("service", "", "Windows service: install|uninstall|start|stop")
)

// ── Windows Service ────────────────────────────────────────────────────────────

var svcConfig = &service.Config{
	Name:        "BatteryMonitor",
	DisplayName: "Battery Monitor 電量監控",
	Description: "即時監控筆電電量、電壓、功率，Web 儀表板 http://localhost:8089",
}

type program struct {
	ctx    context.Context
	cancel context.CancelFunc
	lb     logbuf.Buffer
}

func (p *program) Start(_ service.Service) error { go p.run(); return nil }
func (p *program) Stop(_ service.Service) error  { p.cancel(); return nil }

func (p *program) run() {
	// Redirect global logger to also capture into the in-memory log buffer.
	log.SetOutput(io.MultiWriter(os.Stderr, &p.lb))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Printf("[config] %v — using defaults", err)
		cfg = config.Default()
	}
	log.Printf("[startup] db=%s web=%s", *dbPath, *webAddr)

	var store *storage.Store
	if *dbPath != "" {
		if s, openErr := storage.Open(*dbPath); openErr != nil {
			log.Printf("[storage] %v", openErr)
		} else {
			store = s
			defer s.Close()
		}
	}

	stop := make(chan struct{})
	go func() { <-p.ctx.Done(); close(stop) }()

	m := monitor.New(cfg, &p.lb)
	if store != nil {
		m.SetStore(store)
	}

	if *webAddr != "" {
		srv, srvErr := web.New(*webAddr, cfg)
		if srvErr != nil {
			log.Printf("[web] %v", srvErr)
		} else {
			if store != nil {
				srv.SetStore(store)
			}
			srv.SetLogBuffer(&p.lb)
			m.SetPusher(srv)
			go func() {
				if err := srv.Start(p.ctx); err != nil {
					log.Printf("[web] %v", err)
				}
			}()
		}
	}

	m.Run(stop)
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	flag.Parse()

	// --init
	if *genConfig {
		cfg := config.Default()
		if err := cfg.Save(*cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "error saving config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Default config written to %s\n", *cfgPath)
		return
	}

	// --status
	if *statusOnly {
		printStatus()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	prg := &program{ctx: ctx, cancel: cancel}

	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatal(err)
	}

	// --service install|uninstall|start|stop
	if *svcCtrl != "" {
		if err := service.Control(s, *svcCtrl); err != nil {
			log.Fatalf("service %s: %v", *svcCtrl, err)
		}
		fmt.Printf("Service '%s' completed.\n", *svcCtrl)
		return
	}

	// Running interactively (terminal)
	if service.Interactive() {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config error: %v\n", err)
			os.Exit(1)
		}
		printConfig(cfg)
		if *dbPath != "" {
			fmt.Printf("  Database: %s\n\n", *dbPath)
		}

		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		go func() { <-sigs; fmt.Println("\nShutting down..."); cancel() }()

		prg.run()
		return
	}

	// Running as Windows Service
	if err := s.Run(); err != nil {
		log.Fatal(err)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func printStatus() {
	info, err := battery.Get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(info)

	cap, err := battery.GetCapacity()
	if err == nil && cap.DesignedCapacity > 0 {
		fmt.Printf("Health:  %.1f%% (%d / %d mWh)\n",
			cap.HealthPercent, cap.FullChargedCapacity, cap.DesignedCapacity)
		if cap.CycleCount > 0 {
			fmt.Printf("Cycles:  %d\n", cap.CycleCount)
		}
	}

	rate, rErr := battery.GetRateInfo()
	if rErr != nil {
		fmt.Fprintf(os.Stderr, "GetRateInfo: %v\n", rErr)
	} else {
		if rate.VoltageMV > 0 {
			fmt.Printf("Voltage: %.3f V\n", float64(rate.VoltageMV)/1000.0)
		}
		if rate.RateMW != 0 {
			sign, mw := "+", rate.RateMW
			if mw < 0 {
				sign, mw = "-", -mw
			}
			fmt.Printf("Power:   %s%.3f W  [%s]\n", sign, float64(mw)/1000.0, rate.Source)
		}
		if rate.CapacityMWh > 0 {
			fmt.Printf("Cap now: %d mWh\n", rate.CapacityMWh)
		}
	}
}

func printConfig(cfg *config.Config) {
	fmt.Printf("Battery Monitor starting\n")
	fmt.Printf("  Warn:     %d%% (poll every %s)\n", cfg.WarnLevel, cfg.WarnInterval)
	fmt.Printf("  Critical: %d%% (poll every %s)\n", cfg.CriticalLevel, cfg.CriticalInterval)
	fmt.Printf("  Full:     %d%% notify=%v\n", cfg.FullLevel, cfg.NotifyOnFull)
	fmt.Printf("  Normal poll interval: %s\n", cfg.NormalInterval)
}
