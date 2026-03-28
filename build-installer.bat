@echo off
cd /d "%~dp0"

set WIX=C:\Program Files (x86)\WiX Toolset v3.14\bin\

if not exist "%WIX%candle.exe" goto nowix

echo [1/3] Building battery-monitor.exe ...
go build -ldflags="-s -w" -o battery-monitor.exe .
if errorlevel 1 goto buildfail

echo [2/3] Running candle.exe ...
"%WIX%candle.exe" installer\battery-monitor.wxs -arch arm64 -out installer\battery-monitor.wixobj
if errorlevel 1 goto buildfail

echo [3/3] Running light.exe ...
"%WIX%light.exe" installer\battery-monitor.wixobj -o battery-monitor-setup.msi
if errorlevel 1 goto buildfail

echo.
echo [OK] battery-monitor-setup.msi created.
echo      Dashboard: http://localhost:8089
goto end

:nowix
echo [ERROR] WiX Toolset v3.14 not found at: %WIX%
goto end

:buildfail
echo [ERROR] Build failed.

:end
