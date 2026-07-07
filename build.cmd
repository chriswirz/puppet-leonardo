@echo off
REM Convenience wrapper that runs build.ps1 (frontend export + Go binary).
powershell -NoProfile -ExecutionPolicy Bypass -File "build.ps1" %*
exit /b %ERRORLEVEL%
