@echo off
rem TradeEngine UI launcher — opens the trade engine control panel in a
rem chromeless browser window (no tab bar / no address bar), like a native app.
rem Usage:
rem   trade-engine-ui.bat            -> Chrome app-mode window (recommended)
rem   trade-engine-ui.bat firefox    -> Firefox kiosk window
rem
setlocal

set "URL=http://localhost:8080"
set "CHROME=C:\Program Files\Google\Chrome\Application\chrome.exe"
set "CHROME86=C:\Program Files (x86)\Google\Chrome\Application\chrome.exe"
set "FIREFOX=C:\Program Files\Mozilla Firefox\firefox.exe"
set "FIREFOX86=C:\Program Files (x86)\Mozilla Firefox\firefox.exe"

if /i "%~1"=="firefox" goto firefox

rem ---- default: Chrome app mode (standalone window, no browser chrome) ----
if exist "%CHROME%" (
  start "" "%CHROME%" --app="%URL%" --window-size=520,1000
  goto done
)
if exist "%CHROME86%" (
  start "" "%CHROME86%" --app="%URL%" --window-size=520,1000
  goto done
)

:firefox
if exist "%FIREFOX%" (
  start "" "%FIREFOX%" --kiosk "%URL%"
  goto done
)
if exist "%FIREFOX86%" (
  start "" "%FIREFOX86%" --kiosk "%URL%"
  goto done
)

echo No supported browser found. Open %URL% manually.
:done
endlocal