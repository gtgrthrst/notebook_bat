package battery

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ACStatus represents AC power line status
type ACStatus uint8

const (
	ACOffline  ACStatus = 0
	ACOnline   ACStatus = 1
	ACUnknown  ACStatus = 255
)

func (s ACStatus) String() string {
	switch s {
	case ACOffline:
		return "Discharging"
	case ACOnline:
		return "Plugged in"
	default:
		return "Unknown"
	}
}

// BatteryFlag bit flags from Windows API
const (
	BattFlagHigh      = 1  // > 66%
	BattFlagLow       = 2  // < 33%
	BattFlagCritical  = 4  // < 5%
	BattFlagCharging  = 8
	BattFlagNoBattery = 128
	BattFlagUnknown   = 255
)

// systemPowerStatus mirrors SYSTEM_POWER_STATUS from winbase.h
type systemPowerStatus struct {
	ACLineStatus        uint8
	BatteryFlag         uint8
	BatteryLifePercent  uint8
	SystemStatusFlag    uint8
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	getSystemPowerStatus  = kernel32.NewProc("GetSystemPowerStatus")
)

// Info holds current battery state
type Info struct {
	Percent    int      // 0-100, -1 if unknown
	ACStatus   ACStatus
	Charging   bool
	NoBattery  bool
	SecsLeft   int // seconds remaining, -1 if unknown/charging
}

// TimeLeft returns a human-readable estimate of time remaining
func (i Info) TimeLeft() string {
	if i.Charging || i.NoBattery || i.SecsLeft < 0 {
		return ""
	}
	h := i.SecsLeft / 3600
	m := (i.SecsLeft % 3600) / 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func (i Info) String() string {
	if i.NoBattery {
		return "No battery detected"
	}
	pct := fmt.Sprintf("%d%%", i.Percent)
	if i.Percent < 0 {
		pct = "unknown"
	}
	status := i.ACStatus.String()
	if i.Charging {
		status = "Charging"
	}
	tl := i.TimeLeft()
	if tl != "" {
		return fmt.Sprintf("%s | %s | ~%s remaining", pct, status, tl)
	}
	return fmt.Sprintf("%s | %s", pct, status)
}

// Get reads the current battery state via Windows GetSystemPowerStatus API.
// This is a single syscall — extremely low overhead.
func Get() (Info, error) {
	var status systemPowerStatus
	r, _, err := getSystemPowerStatus.Call(uintptr(unsafe.Pointer(&status)))
	if r == 0 {
		return Info{}, fmt.Errorf("GetSystemPowerStatus failed: %w", err)
	}

	info := Info{
		ACStatus:  ACStatus(status.ACLineStatus),
		NoBattery: status.BatteryFlag == BattFlagNoBattery,
	}

	if status.BatteryLifePercent == 255 {
		info.Percent = -1
	} else {
		info.Percent = int(status.BatteryLifePercent)
	}

	info.Charging = status.BatteryFlag&BattFlagCharging != 0

	if status.BatteryLifeTime == 0xFFFFFFFF {
		info.SecsLeft = -1
	} else {
		info.SecsLeft = int(status.BatteryLifeTime)
	}

	return info, nil
}
