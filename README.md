# notebook_bat — Laptop Battery Monitor

> Windows ARM (Qualcomm Snapdragon) 筆電電量監控服務
> A real-time battery monitoring service for Windows ARM laptops

---

## 目錄 / Table of Contents

- [功能特色 / Features](#功能特色--features)
- [系統需求 / Requirements](#系統需求--requirements)
- [安裝與啟動 / Installation](#安裝與啟動--installation)
- [Web 儀表板 / Dashboard](#web-儀表板--dashboard)
- [命令列參數 / CLI Flags](#命令列參數--cli-flags)
- [從原始碼建置 / Build from Source](#從原始碼建置--build-from-source)
- [建置 MSI 安裝程式 / Build MSI Installer](#建置-msi-安裝程式--build-msi-installer)
- [架構說明 / Architecture](#架構說明--architecture)

---

## 功能特色 / Features

**繁體中文**
- 即時電量百分比、充放電狀態、預估剩餘時間
- 電池健康度（設計容量 vs 實際滿充容量）、循環次數
- 即時電壓（mV）、充放電功率（mW）——透過 WMI 取得，相容 Qualcomm ARM 驅動
- SQLite 永久記錄，支援歷史報表
- 充放電率棒棒圖、電壓折線圖
- 程序 CPU 佔用排行與時間軸
- 低電量 / 充飽 Windows 通知
- 可安裝為 Windows Service（開機自動啟動）

**English**
- Real-time battery percentage, charge status, and estimated time remaining
- Battery health (design vs full-charge capacity) and cycle count
- Live voltage (mV) and charge/discharge rate (mW) — via WMI, compatible with Qualcomm ARM drivers
- Persistent SQLite storage with historical reports
- Charge/discharge lollipop chart and voltage line chart
- Per-process CPU usage ranking and timeline
- Low battery / fully charged Windows toast notifications
- Installable as a Windows Service (auto-start on boot)

---

## 系統需求 / Requirements

| 項目 | 規格 |
|------|------|
| OS   | Windows 11 (ARM64 / x64) |
| Go   | 1.21+ |
| WiX  | Toolset v3.14（僅建置 MSI 需要 / MSI build only） |

---

## 安裝與啟動 / Installation

### 方法一：MSI 安裝程式 / Method 1: MSI Installer

從 [Releases](https://github.com/gtgrthrst/notebook_bat/releases) 下載 `battery-monitor-setup.msi` 並執行。
Download `battery-monitor-setup.msi` from [Releases](https://github.com/gtgrthrst/notebook_bat/releases) and run it.

安裝後服務自動啟動，開啟瀏覽器造訪：
After installation the service starts automatically. Open:

```
http://localhost:8089
```

解除安裝請使用「控制台 → 程式和功能」，服務會自動停止並移除。
To uninstall, use "Control Panel → Programs and Features". The service is stopped and removed automatically.

---

### 方法二：手動安裝（排程工作）/ Method 2: Scheduled Task

以系統管理員身分執行 / Run as Administrator:

```powershell
# 安裝 / Install
powershell -ExecutionPolicy Bypass -File install.ps1

# 解除安裝 / Uninstall
powershell -ExecutionPolicy Bypass -File uninstall.ps1
```

---

### 方法三：直接執行 / Method 3: Run Directly

```powershell
.\battery-monitor.exe
```

---

## Web 儀表板 / Dashboard

啟動後開啟瀏覽器 / Open browser after starting:

```
http://localhost:8089
```

| 分頁 / Tab | 內容 / Content |
|-----------|---------------|
| 即時狀態 / Live Status | 電量圓規、充電狀態、電量歷史折線圖 |
| 功率/電壓 / Power & Voltage | 即時電壓/功率、充放電率棒棒圖、電壓歷史圖 |
| 程序 CPU / Process CPU | 各程序 CPU 佔用排行、時間軸圖表 |
| 電池報表 / Battery Report | 每日統計、電池健康趨勢、容量歷史 |

---

## 命令列參數 / CLI Flags

```
battery-monitor.exe [flags]

Flags:
  --web      string   Web dashboard address (default ":8089")
  --db       string   SQLite database path  (default "battery.db")
  --config   string   Config file path      (default "config.json")
  --status           Print battery status and exit
  --init             Write default config.json and exit
  --service  string  Windows service control: install | uninstall | start | stop
```

**範例 / Examples**

```powershell
# 查看目前電量
.\battery-monitor.exe --status

# 產生預設設定檔
.\battery-monitor.exe --init

# 安裝為 Windows Service
.\battery-monitor.exe --service install

# 自訂 port 及資料庫路徑
.\battery-monitor.exe --web :9000 --db C:\data\battery.db
```

---

## 從原始碼建置 / Build from Source

```powershell
git clone https://github.com/gtgrthrst/notebook_bat.git
cd notebook_bat
go mod tidy
go build -o battery-monitor.exe .
```

---

## 建置 MSI 安裝程式 / Build MSI Installer

需先安裝 [WiX Toolset v3.14](https://github.com/wixtoolset/wix3/releases)。
Requires [WiX Toolset v3.14](https://github.com/wixtoolset/wix3/releases).

```powershell
.\build-installer.bat
# 輸出 / Output: battery-monitor-setup.msi
```

---

## 記錄頻率 / Recording Frequency

監控迴圈依電量自動調整輪詢間隔，並在每次輪詢後將資料寫入 SQLite。
The monitor loop adapts its polling interval based on battery level and writes to SQLite on every poll.

| 電量狀態 / State | 條件 / Condition | 間隔 / Interval | 每小時筆數 / Records/hr |
|----------------|-----------------|----------------|------------------------|
| 正常 Normal     | > 30 %          | 60 秒 / 60 s   | 60                     |
| 低電 Warn       | ≤ 30 %          | 30 秒 / 30 s   | 120                    |
| 危急 Critical   | ≤ 15 %          | 15 秒 / 15 s   | 240                    |

每筆記錄包含 / Each record contains:

| 欄位 / Field    | 說明 / Description              |
|----------------|--------------------------------|
| `percent`      | 電量百分比 Battery percentage    |
| `ac`           | 是否接電源 AC connected          |
| `charging`     | 是否充電中 Charging status        |
| `secs_left`    | 預估剩餘秒數 Estimated seconds left |
| `voltage_mv`   | 電壓 mV（WMI）                   |
| `rate_mw`      | 功率 mW，正=充電，負=放電           |
| `cap_now_mwh`  | 目前容量 mWh Current capacity     |
| `full_cap`     | 滿充容量 mWh Full charge capacity |
| `designed_cap` | 設計容量 mWh Design capacity      |
| `cycle_count`  | 循環次數 Cycle count              |

程序快照（`proc_snapshots`）與電量記錄同頻率，每筆記錄前 15 名 CPU 佔用程序。
Process snapshots (`proc_snapshots`) are recorded at the same frequency, capturing the top 15 CPU-consuming processes per poll.

**自訂間隔 / Customise intervals**

執行 `.\battery-monitor.exe --init` 產生 `config.json` 後修改：
Run `.\battery-monitor.exe --init` to generate `config.json`, then edit:

```json
{
  "normal_interval":   "60s",
  "warn_interval":     "30s",
  "critical_interval": "15s",
  "warn_level":        30,
  "critical_level":    15
}
```

---

## 架構說明 / Architecture

```
notebook_bat/
├── main.go              # 程式進入點、Windows Service 整合
├── battery/
│   ├── battery.go       # GetSystemPowerStatus (kernel32)
│   ├── capacity.go      # IOCTL battery query + NtQuerySystemInformation
│   └── wmi.go           # WMI fallback (BatteryStatus / BatteryStaticData)
├── monitor/
│   └── monitor.go       # 主監控迴圈、通知、資料儲存
├── storage/
│   └── storage.go       # SQLite 讀寫、歷史查詢
├── sysmon/
│   └── sysmon.go        # 程序 CPU/記憶體取樣 (pure syscall)
├── web/
│   ├── server.go        # HTTP + SSE 伺服器
│   └── static/
│       └── index.html   # 單頁儀表板 (Canvas 圖表)
├── config/
│   └── config.go        # 設定檔讀寫
├── notifier/
│   └── notifier.go      # Windows Toast 通知
└── installer/
    └── battery-monitor.wxs  # WiX MSI 安裝腳本
```

**資料流 / Data Flow**

```
battery.Get() / GetCapacity() / GetRateInfo()
        ↓
   monitor.Run()  ──→  storage.Record()  ──→  SQLite DB
        ↓
   web.Push()  ──→  SSE stream  ──→  Browser (index.html)
```

---

## License

MIT
