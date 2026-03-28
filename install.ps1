# install.ps1 — Register battery monitor as a Windows Scheduled Task
# Run once as Administrator: powershell -ExecutionPolicy Bypass -File install.ps1

$ErrorActionPreference = "Stop"
$dir  = Split-Path -Parent $MyInvocation.MyCommand.Path
$exe  = Join-Path $dir "battery-monitor.exe"
$task = "BatteryMonitor"

if (-not (Test-Path $exe)) {
    Write-Error "battery-monitor.exe not found in $dir. Build it first with: go build -o battery-monitor.exe ."
    exit 1
}

# Remove old task if it exists
if (Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $task -Confirm:$false
    Write-Host "Removed old task."
}

$action  = New-ScheduledTaskAction -Execute $exe -WorkingDirectory $dir
$trigger = New-ScheduledTaskTrigger -AtLogOn
$settings = New-ScheduledTaskSettingsSet `
    -ExecutionTimeLimit (New-TimeSpan -Hours 0) `
    -RestartCount 3 `
    -RestartInterval (New-TimeSpan -Minutes 1) `
    -StartWhenAvailable

Register-ScheduledTask `
    -TaskName $task `
    -Action $action `
    -Trigger $trigger `
    -Settings $settings `
    -RunLevel Limited `
    -Description "Low-power battery monitor with Windows notifications" | Out-Null

Write-Host "Installed '$task' — will start automatically at next login."
Write-Host "To start now: Start-ScheduledTask -TaskName '$task'"
Start-ScheduledTask -TaskName $task
Write-Host "Started."
