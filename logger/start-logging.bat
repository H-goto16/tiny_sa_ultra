@echo off
chcp 65001 >nul
cd /d %~dp0
:loop
tinysa-logger.exe log -interval 60 -out logs
echo.
echo exited. restart in 10s (close window to quit)
timeout /t 10
goto loop
