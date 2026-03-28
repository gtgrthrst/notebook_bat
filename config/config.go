package config

import (
	"encoding/json"
	"os"
	"time"
)

type Config struct {
	// Alert thresholds (percent)
	WarnLevel     int `json:"warn_level"`     // default 30
	CriticalLevel int `json:"critical_level"` // default 15
	FullLevel     int `json:"full_level"`     // notify when charged above this, default 95

	// Polling intervals
	NormalInterval   Duration `json:"normal_interval"`   // default 60s
	WarnInterval     Duration `json:"warn_interval"`     // default 30s
	CriticalInterval Duration `json:"critical_interval"` // default 15s

	// Behavior
	NotifyOnFull      bool `json:"notify_on_full"`       // alert when fully charged
	NotifyOnUnplug    bool `json:"notify_on_unplug"`     // alert when AC unplugged
	NotifyOnPlug      bool `json:"notify_on_plug"`       // alert when AC plugged in
	SuppressRepeats   bool `json:"suppress_repeats"`     // don't repeat same alert
	LogToFile         bool `json:"log_to_file"`          // write log to battery.log
}

// Duration wraps time.Duration for JSON marshaling
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

func Default() *Config {
	return &Config{
		WarnLevel:        30,
		CriticalLevel:    15,
		FullLevel:        95,
		NormalInterval:   Duration{60 * time.Second},
		WarnInterval:     Duration{30 * time.Second},
		CriticalInterval: Duration{15 * time.Second},
		NotifyOnFull:     true,
		NotifyOnUnplug:   true,
		NotifyOnPlug:     false,
		SuppressRepeats:  true,
		LogToFile:        false,
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // use defaults if no config file
		}
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
