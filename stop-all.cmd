@echo off
setlocal
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0stop-all.ps1" %*
set "stop_result=%errorlevel%"
if not "%stop_result%"=="0" pause
exit /b %stop_result%
