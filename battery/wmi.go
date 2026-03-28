package battery

import (
	"fmt"

	"github.com/yusufpapurcu/wmi"
)

func init() {
	// Allow WMI result fields that have no matching struct field to be silently
	// ignored. Required for Win32_PerfRawData subclasses (BatteryStaticData,
	// BatteryFullChargedCapacity, BatteryCycleCount) which return many extra
	// Granularity*/Timestamp* properties not present in our minimal structs.
	wmi.DefaultClient.AllowMissingFields = true
}

// wmiBattStaticData mirrors root\wmi.BatteryStaticData.
// Only maps fields we actually use; unmapped WMI properties are ignored.
type wmiBattStaticData struct {
	DesignedCapacity uint32
}

// wmiBattCycleCount mirrors root\wmi.BatteryCycleCount.
type wmiBattCycleCount struct {
	CycleCount uint32
}

// wmiBattFullCharge mirrors root\wmi.BatteryFullChargedCapacity.
type wmiBattFullCharge struct {
	FullChargedCapacity uint32
}

// getCapacityFromWMI queries WMI for battery design capacity, full charge
// capacity, and cycle count. Works on Qualcomm ARM laptops where IOCTL fails.
//
// BatteryStaticData / BatteryFullChargedCapacity / BatteryCycleCount are
// Win32_PerfRawData subclasses; they require "SELECT *" — field-specific
// SELECT queries are rejected by the ACPI WMI provider on some ARM drivers.
func getCapacityFromWMI() (CapacityInfo, error) {
	var staticRows []wmiBattStaticData
	if err := wmi.QueryNamespace("SELECT * FROM BatteryStaticData", &staticRows, `root\wmi`); err != nil {
		return CapacityInfo{}, fmt.Errorf("WMI BatteryStaticData: %w", err)
	}
	if len(staticRows) == 0 {
		return CapacityInfo{}, fmt.Errorf("WMI BatteryStaticData: no instances")
	}

	var fullRows []wmiBattFullCharge
	if err := wmi.QueryNamespace("SELECT * FROM BatteryFullChargedCapacity", &fullRows, `root\wmi`); err != nil {
		return CapacityInfo{}, fmt.Errorf("WMI BatteryFullChargedCapacity: %w", err)
	}
	if len(fullRows) == 0 {
		return CapacityInfo{}, fmt.Errorf("WMI BatteryFullChargedCapacity: no instances")
	}

	info := CapacityInfo{
		DesignedCapacity:    int(staticRows[0].DesignedCapacity),
		FullChargedCapacity: int(fullRows[0].FullChargedCapacity),
	}
	if info.DesignedCapacity > 0 {
		info.HealthPercent = float64(info.FullChargedCapacity) / float64(info.DesignedCapacity) * 100.0
	}

	// CycleCount lives in a separate WMI class.
	var cycleRows []wmiBattCycleCount
	if err := wmi.QueryNamespace("SELECT * FROM BatteryCycleCount", &cycleRows, `root\wmi`); err == nil && len(cycleRows) > 0 {
		info.CycleCount = int(cycleRows[0].CycleCount)
	}

	return info, nil
}

// wmiBattStatus mirrors the WMI BatteryStatus class in root\wmi.
// Field names must match WMI property names exactly (case-insensitive).
type wmiBattStatus struct {
	ChargeRate        uint32
	DischargeRate     uint32
	Voltage           uint32 // mV
	RemainingCapacity uint32 // mWh
	Charging          bool
	Discharging       bool
	PowerOnline       bool
}

// getRateFromWMI queries root\wmi.BatteryStatus via the WMI COM interface.
// This works reliably on ARM-based Windows laptops (e.g. Qualcomm Snapdragon)
// where the standard battery IOCTL returns BATTERY_UNKNOWN_RATE.
//
// WMI property units (as documented by Microsoft and validated empirically):
//   - Voltage           : millivolts (mV)
//   - RemainingCapacity : milliwatt-hours (mWh)
//   - ChargeRate        : milliwatts (mW), > 0 when charging
//   - DischargeRate     : milliwatts (mW), > 0 when discharging (positive value)
func getRateFromWMI() (RateInfo, error) {
	var dst []wmiBattStatus
	// BatteryStatus is a Win32_PerfRawData subclass; the ACPI WMI provider on
	// Qualcomm ARM rejects field-selective SELECTs — use SELECT * instead.
	if err := wmi.QueryNamespace("SELECT * FROM BatteryStatus", &dst, `root\wmi`); err != nil {
		return RateInfo{}, fmt.Errorf("WMI BatteryStatus: %w", err)
	}
	if len(dst) == 0 {
		return RateInfo{}, fmt.Errorf("WMI BatteryStatus: no instances found")
	}

	s := dst[0]
	info := RateInfo{
		VoltageMV:     int(s.Voltage),
		CapacityMWh:   int(s.RemainingCapacity),
		IsCharging:    s.Charging,
		IsDischarging: s.Discharging,
		Source:        "wmi",
	}

	// Convention: RateMW > 0 = charging, < 0 = discharging.
	switch {
	case s.Charging && s.ChargeRate > 0:
		info.RateMW = int(s.ChargeRate)
	case s.Discharging && s.DischargeRate > 0:
		info.RateMW = -int(s.DischargeRate)
	}

	return info, nil
}
