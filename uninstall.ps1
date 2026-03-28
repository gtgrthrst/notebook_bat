# uninstall.ps1 — Remove the scheduled task
$task = "BatteryMonitor"
Stop-ScheduledTask  -TaskName $task -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName $task -Confirm:$false -ErrorAction SilentlyContinue
Write-Host "Removed '$task'."
