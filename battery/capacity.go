package battery

import (
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// winGUID mirrors the Windows GUID struct.
type winGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// GUID_DEVCLASS_BATTERY = {72631E54-78A4-11D0-BCF7-00AA00B7B32A}
var guidBattery = winGUID{
	0x72631e54, 0x78a4, 0x11d0,
	[8]byte{0xbc, 0xf7, 0x00, 0xaa, 0x00, 0xb7, 0xb3, 0x2a},
}

const (
	digcfPresent         uintptr = 0x00000002
	digcfDeviceInterface uintptr = 0x00000010

	ioctlBatteryQueryTag         uintptr = 0x00294040
	ioctlBatteryQueryInformation uintptr = 0x00294044
	ioctlBatteryQueryStatus      uintptr = 0x0029404C
	batteryInfoLevel             uint32  = 1

	capGenericReadWrite = 0x80000000 | 0x40000000
	capFileShareRW      = 0x00000001 | 0x00000002
	capOpenExisting     = 3
	capFileAttrNormal   = 0x00000080
	capInvalidHandle    = ^uintptr(0)

	batteryUnknownCapacity uint32 = 0xFFFFFFFF
	batteryUnknownVoltage  uint32 = 0xFFFFFFFF
	batteryUnknownRate     int32  = -2147483648

	// PowerState bit flags
	BattPowerOnLine  = 0x1
	BattDischarging  = 0x2
	BattCharging     = 0x4
	BattCriticalFlag = 0x8
)

// ── Windows API procs ─────────────────────────────────────────────────────────

var (
	ntdll                        = syscall.NewLazyDLL("ntdll.dll")
	procNtQuerySystemInformation = ntdll.NewProc("NtQuerySystemInformation")

	setupapi                            = syscall.NewLazyDLL("setupapi.dll")
	procSetupDiGetClassDevs             = setupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces     = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetail = setupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList    = setupapi.NewProc("SetupDiDestroyDeviceInfoList")
	procCreateFile                      = kernel32.NewProc("CreateFileW")
	procDeviceIoControl                 = kernel32.NewProc("DeviceIoControl")
	procCloseHandle                     = kernel32.NewProc("CloseHandle")
)

// ── Structs ───────────────────────────────────────────────────────────────────

// ntBattState mirrors SYSTEM_BATTERY_STATE (NtQuerySystemInformation class 5).
// Layout (x64): 8 flag bytes + 2×uint64 + 4×uint32 = 40 bytes.
type ntBattState struct {
	AcOnLine          uint8
	BatteryPresent    uint8
	Charging          uint8
	Discharging       uint8
	Spare1            [3]uint8
	Tag               uint8
	MaxCapacity       uint64 // mWh (or mAh on some ACPI batteries)
	RemainingCapacity uint64 // mWh
	Rate              int32  // mW; positive=charging, negative=discharging; 0x80000000=unknown
	EstimatedTime     uint32
	DefaultAlert1     uint32
	DefaultAlert2     uint32
}

type spDeviceInterfaceData struct {
	CbSize             uint32
	InterfaceClassGUID winGUID
	Flags              uint32
	Reserved           uintptr
}

type batteryQueryInfo struct {
	BatteryTag       uint32
	InformationLevel uint32
	AtRate           int32
}

type batteryInfo struct {
	Capabilities        uint32
	Technology          uint8
	Reserved            [3]uint8
	Chemistry           [4]uint8
	DesignedCapacity    uint32
	FullChargedCapacity uint32
	DefaultAlert1       uint32
	DefaultAlert2       uint32
	CriticalBias        uint32
	CycleCount          uint32
}

type batteryWaitStatus struct {
	BatteryTag   uint32
	Timeout      uint32
	PowerState   uint32
	LowCapacity  uint32
	HighCapacity uint32
}

type batteryRawStatus struct {
	PowerState uint32
	Capacity   uint32
	Voltage    uint32
	Rate       int32
}

// ── Exported types ────────────────────────────────────────────────────────────

// CapacityInfo holds design / full-charge capacity and battery health.
type CapacityInfo struct {
	DesignedCapacity    int     // mWh
	FullChargedCapacity int     // mWh
	CycleCount          int
	HealthPercent       float64
}

// RateInfo holds real-time electrical measurements.
// Source indicates which API provided the data.
type RateInfo struct {
	VoltageMV     int    // millivolts (0 = unknown)
	RateMW        int    // milliwatts; positive=charging, negative=discharging (0 = unknown)
	CapacityMWh   int    // current charge in mWh (0 = unknown)
	PowerState    uint32
	IsCharging    bool
	IsDischarging bool
	Source        string // "ioctl" | "ntquery" | "estimated" | ""
}

// ── Caches ────────────────────────────────────────────────────────────────────

var capCache struct {
	mu      sync.Mutex
	info    CapacityInfo
	fetchAt time.Time
	ttl     time.Duration
}

func init() { capCache.ttl = 10 * time.Minute }

// ── NtQuerySystemInformation path ────────────────────────────────────────────

const ntSystemBatteryState = 5

// getNtBattState queries the Windows kernel for the aggregate battery state.
// This is more reliable than IOCTL_BATTERY_QUERY_STATUS on many modern laptops
// whose ACPI drivers return BATTERY_UNKNOWN_RATE for the per-battery IOCTL.
func getNtBattState() (ntBattState, error) {
	var bs ntBattState
	ret, _, _ := procNtQuerySystemInformation.Call(
		ntSystemBatteryState,
		uintptr(unsafe.Pointer(&bs)),
		unsafe.Sizeof(bs),
		0,
	)
	if ret != 0 {
		return ntBattState{}, fmt.Errorf("NtQuerySystemInformation status=0x%08x", ret)
	}
	if bs.BatteryPresent == 0 {
		return ntBattState{}, fmt.Errorf("no battery present")
	}
	return bs, nil
}

// ── SetupDi / IOCTL path ──────────────────────────────────────────────────────

func openBatteryHandle() (hBat uintptr, tag uint32, cleanup func(), err error) {
	hdev, _, e := procSetupDiGetClassDevs.Call(
		uintptr(unsafe.Pointer(&guidBattery)), 0, 0,
		digcfPresent|digcfDeviceInterface,
	)
	if hdev == capInvalidHandle {
		return 0, 0, nil, fmt.Errorf("SetupDiGetClassDevs: %w", e)
	}

	devicePath, pathErr := getFirstBatteryPath(hdev)
	procSetupDiDestroyDeviceInfoList.Call(hdev)
	if pathErr != nil {
		return 0, 0, nil, pathErr
	}

	pathPtr, _ := syscall.UTF16PtrFromString(devicePath)
	hBat, _, e = procCreateFile.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		capGenericReadWrite, capFileShareRW,
		0, capOpenExisting, capFileAttrNormal, 0,
	)
	if hBat == capInvalidHandle {
		return 0, 0, nil, fmt.Errorf("CreateFile(%s): %w", devicePath, e)
	}

	var dwWait, dwTag, dwOut uint32
	r, _, e := procDeviceIoControl.Call(
		hBat, ioctlBatteryQueryTag,
		uintptr(unsafe.Pointer(&dwWait)), unsafe.Sizeof(dwWait),
		uintptr(unsafe.Pointer(&dwTag)), unsafe.Sizeof(dwTag),
		uintptr(unsafe.Pointer(&dwOut)), 0,
	)
	if r == 0 {
		procCloseHandle.Call(hBat)
		return 0, 0, nil, fmt.Errorf("IOCTL_BATTERY_QUERY_TAG: %w", e)
	}

	return hBat, dwTag, func() { procCloseHandle.Call(hBat) }, nil
}

func getFirstBatteryPath(hdev uintptr) (string, error) {
	var did spDeviceInterfaceData
	did.CbSize = uint32(unsafe.Sizeof(did))
	r, _, e := procSetupDiEnumDeviceInterfaces.Call(
		hdev, 0,
		uintptr(unsafe.Pointer(&guidBattery)),
		0, uintptr(unsafe.Pointer(&did)),
	)
	if r == 0 {
		return "", fmt.Errorf("SetupDiEnumDeviceInterfaces: %w", e)
	}

	var cbRequired uint32
	procSetupDiGetDeviceInterfaceDetail.Call(
		hdev, uintptr(unsafe.Pointer(&did)),
		0, 0, uintptr(unsafe.Pointer(&cbRequired)), 0,
	)
	if cbRequired < 8 {
		cbRequired = 512
	}

	buf := make([]byte, cbRequired+4)
	*(*uint32)(unsafe.Pointer(&buf[0])) = 8 // cbSize on x64
	r, _, e = procSetupDiGetDeviceInterfaceDetail.Call(
		hdev, uintptr(unsafe.Pointer(&did)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(cbRequired),
		uintptr(unsafe.Pointer(&cbRequired)), 0,
	)
	if r == 0 {
		return "", fmt.Errorf("SetupDiGetDeviceInterfaceDetail: %w", e)
	}

	nWords := (len(buf) - 4) / 2
	return syscall.UTF16ToString(
		unsafe.Slice((*uint16)(unsafe.Pointer(&buf[4])), nWords),
	), nil
}

func doQueryCapacity(hBat uintptr, tag uint32) (CapacityInfo, error) {
	bqi := batteryQueryInfo{BatteryTag: tag, InformationLevel: batteryInfoLevel}
	var bi batteryInfo
	var dwOut uint32
	r, _, e := procDeviceIoControl.Call(
		hBat, ioctlBatteryQueryInformation,
		uintptr(unsafe.Pointer(&bqi)), unsafe.Sizeof(bqi),
		uintptr(unsafe.Pointer(&bi)), unsafe.Sizeof(bi),
		uintptr(unsafe.Pointer(&dwOut)), 0,
	)
	if r == 0 {
		return CapacityInfo{}, fmt.Errorf("IOCTL_BATTERY_QUERY_INFORMATION: %w", e)
	}
	cap := CapacityInfo{
		DesignedCapacity:    int(bi.DesignedCapacity),
		FullChargedCapacity: int(bi.FullChargedCapacity),
		CycleCount:          int(bi.CycleCount),
	}
	if bi.DesignedCapacity > 0 {
		cap.HealthPercent = float64(bi.FullChargedCapacity) / float64(bi.DesignedCapacity) * 100.0
	}
	return cap, nil
}

func doQueryIoctlStatus(hBat uintptr, tag uint32) (RateInfo, error) {
	bws := batteryWaitStatus{
		BatteryTag:   tag,
		Timeout:      0,
		HighCapacity: batteryUnknownCapacity,
	}
	var bs batteryRawStatus
	var dwOut uint32
	r, _, e := procDeviceIoControl.Call(
		hBat, ioctlBatteryQueryStatus,
		uintptr(unsafe.Pointer(&bws)), unsafe.Sizeof(bws),
		uintptr(unsafe.Pointer(&bs)), unsafe.Sizeof(bs),
		uintptr(unsafe.Pointer(&dwOut)), 0,
	)
	if r == 0 {
		return RateInfo{}, fmt.Errorf("IOCTL_BATTERY_QUERY_STATUS: %w", e)
	}

	info := RateInfo{
		PowerState:    bs.PowerState,
		IsCharging:    bs.PowerState&BattCharging != 0,
		IsDischarging: bs.PowerState&BattDischarging != 0,
		Source:        "ioctl",
	}
	if bs.Voltage != batteryUnknownVoltage {
		info.VoltageMV = int(bs.Voltage)
	}
	if bs.Rate != batteryUnknownRate {
		info.RateMW = int(bs.Rate)
	}
	if bs.Capacity != batteryUnknownCapacity {
		info.CapacityMWh = int(bs.Capacity)
	}
	return info, nil
}

// ── Exported API ──────────────────────────────────────────────────────────────

// GetCapacity returns battery capacity info, cached for 10 minutes.
// It tries IOCTL first, then WMI as fallback (required on ARM/Qualcomm).
func GetCapacity() (CapacityInfo, error) {
	capCache.mu.Lock()
	defer capCache.mu.Unlock()
	if time.Since(capCache.fetchAt) < capCache.ttl {
		return capCache.info, nil
	}

	// Try IOCTL path first.
	var info CapacityInfo
	if hBat, tag, cleanup, err := openBatteryHandle(); err == nil {
		if ci, err := doQueryCapacity(hBat, tag); err == nil {
			info = ci
		}
		cleanup()
	}

	// Fall back to WMI when IOCTL returned no useful data (ARM/Qualcomm).
	if info.DesignedCapacity == 0 {
		if wmiInfo, err := getCapacityFromWMI(); err == nil {
			info = wmiInfo
		}
	}

	if info.DesignedCapacity == 0 {
		return CapacityInfo{}, fmt.Errorf("GetCapacity: no data from IOCTL or WMI")
	}
	capCache.info = info
	capCache.fetchAt = time.Now()
	return info, nil
}

// GetRateInfo returns real-time voltage, charge/discharge rate and current
// capacity. It tries three sources in priority order and returns the first
// that yields a non-zero rate or voltage:
//
//  1. NtQuerySystemInformation(SystemBatteryState) — fast kernel call,
//     works on most x86/x64 laptops.
//  2. IOCTL_BATTERY_QUERY_STATUS — per-battery device ioctl, supplements
//     voltage when NtQuery has no voltage.
//  3. WMI root\wmi.BatteryStatus — most compatible, works on ARM/Qualcomm
//     laptops; ~10–50 ms overhead per call.
//
// RateInfo.Source reports which API provided the Rate value.
func GetRateInfo() (RateInfo, error) {
	var best RateInfo

	// ── Source 1: NtQuerySystemInformation ────────────────────────────────
	ntbs, ntErr := getNtBattState()
	if ntErr == nil {
		best.IsCharging = ntbs.Charging != 0
		best.IsDischarging = ntbs.Discharging != 0
		if ntbs.Rate != batteryUnknownRate && ntbs.Rate != 0 {
			best.RateMW = int(ntbs.Rate)
			best.Source = "ntquery"
		}
		if ntbs.RemainingCapacity > 0 && ntbs.RemainingCapacity < 0xFFFFFFFFFFFFFFFF {
			best.CapacityMWh = int(ntbs.RemainingCapacity)
		}
	}

	// ── Source 2: IOCTL ───────────────────────────────────────────────────
	// Runs regardless of NtQuery result to fill in Voltage, and to override
	// Rate/Capacity when NtQuery returned nothing useful.
	if hBat, tag, cleanup, ioctlErr := openBatteryHandle(); ioctlErr == nil {
		defer cleanup()
		if rs, sErr := doQueryIoctlStatus(hBat, tag); sErr == nil {
			if rs.VoltageMV > 0 {
				best.VoltageMV = rs.VoltageMV
			}
			if best.RateMW == 0 && rs.RateMW != 0 {
				best.RateMW = rs.RateMW
				best.Source = "ioctl"
			}
			if best.CapacityMWh == 0 && rs.CapacityMWh > 0 {
				best.CapacityMWh = rs.CapacityMWh
			}
			if !best.IsCharging && !best.IsDischarging {
				best.IsCharging = rs.IsCharging
				best.IsDischarging = rs.IsDischarging
			}
		}
	}

	// ── Source 3: WMI root\wmi.BatteryStatus ──────────────────────────────
	// Used when sources 1 & 2 return no Rate (common on ARM/Qualcomm laptops
	// whose ACPI battery drivers bypass the standard Windows battery miniclass).
	if best.RateMW == 0 || best.VoltageMV == 0 {
		if wmiInfo, wmiErr := getRateFromWMI(); wmiErr == nil {
			if best.RateMW == 0 && wmiInfo.RateMW != 0 {
				best.RateMW = wmiInfo.RateMW
				best.Source = "wmi"
			}
			if best.VoltageMV == 0 && wmiInfo.VoltageMV > 0 {
				best.VoltageMV = wmiInfo.VoltageMV
			}
			if best.CapacityMWh == 0 && wmiInfo.CapacityMWh > 0 {
				best.CapacityMWh = wmiInfo.CapacityMWh
			}
			if !best.IsCharging && !best.IsDischarging {
				best.IsCharging = wmiInfo.IsCharging
				best.IsDischarging = wmiInfo.IsDischarging
			}
		}
	}

	if best.Source == "" && (best.RateMW != 0 || best.VoltageMV != 0) {
		best.Source = "ntquery"
	}

	// Return best-effort result; callers tolerate partial data.
	if best.RateMW == 0 && best.VoltageMV == 0 && best.CapacityMWh == 0 {
		if ntErr != nil {
			return RateInfo{}, fmt.Errorf("GetRateInfo: all sources failed (NtQuery: %v)", ntErr)
		}
	}
	return best, nil
}
