package notifier

import (
	"fmt"
	"os/exec"
)

type Level int

const (
	Info Level = iota
	Warn
	Critical
)

// toastPS builds a PowerShell one-liner that shows a Windows balloon notification.
// Works on Windows 10/11 without any external dependencies.
func toastPS(title, message string, level Level) string {
	icon := "Info"
	switch level {
	case Warn:
		icon = "Warning"
	case Critical:
		icon = "Error"
	}
	// Uses Windows Forms NotifyIcon — visible ~5 seconds then auto-dismisses
	return fmt.Sprintf(`
Add-Type -AssemblyName System.Windows.Forms;
$n = New-Object System.Windows.Forms.NotifyIcon;
$n.Icon = [System.Drawing.SystemIcons]::%s;
$n.BalloonTipIcon = '%s';
$n.BalloonTipTitle = '%s';
$n.BalloonTipText = '%s';
$n.Visible = $true;
$n.ShowBalloonTip(5000);
Start-Sleep -Milliseconds 5500;
$n.Dispose()
`, icon, icon, title, message)
}

// Send shows a Windows balloon notification (non-blocking — runs PowerShell in background).
func Send(title, message string, level Level) {
	ps := toastPS(title, message, level)
	// -WindowStyle Hidden prevents a console window from flashing
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", ps)
	_ = cmd.Start() // fire-and-forget
}

// Alert helpers

func LowBattery(percent int) {
	Send(
		"Battery Low",
		fmt.Sprintf("Battery at %d%% — please plug in your charger.", percent),
		Warn,
	)
}

func CriticalBattery(percent int) {
	Send(
		"Critical Battery",
		fmt.Sprintf("Battery at %d%% — connect charger immediately!", percent),
		Critical,
	)
}

func FullyCharged(percent int) {
	Send(
		"Battery Full",
		fmt.Sprintf("Battery is at %d%%. Consider unplugging to preserve battery health.", percent),
		Info,
	)
}

func Unplugged(percent int) {
	Send(
		"AC Unplugged",
		fmt.Sprintf("Running on battery — %d%% remaining.", percent),
		Info,
	)
}

func PluggedIn(percent int) {
	Send(
		"AC Connected",
		fmt.Sprintf("Charging — currently at %d%%.", percent),
		Info,
	)
}
