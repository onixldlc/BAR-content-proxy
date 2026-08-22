@echo off
REM ---------------------------------------------------------------------
REM Launch Beyond All Reason with downloads routed through BAR-proxy.
REM
REM Nothing is installed and nothing is changed permanently: setlocal keeps
REM these variables inside this script, so they apply to the game process
REM and nothing else. Close it and you are back to stock behaviour.
REM
REM EDIT THE TWO LINES BELOW.
REM ---------------------------------------------------------------------

setlocal

REM 1. Where your proxy lives. Scheme and port included, no trailing slash.
set "PROXY=http://your-proxy:8080"

REM 2. Where BAR is installed. Leave empty to try the usual locations.
set "BAR_EXE="

REM ---------------------------------------------------------------------

set "PRD_RAPID_REPO_MASTER=%PROXY%/repos.gz"
set "PRD_HTTP_SEARCH_URL=%PROXY%/find"

if not "%BAR_EXE%"=="" goto launch

if exist "%LOCALAPPDATA%\Programs\Beyond-All-Reason\Beyond-All-Reason.exe" (
    set "BAR_EXE=%LOCALAPPDATA%\Programs\Beyond-All-Reason\Beyond-All-Reason.exe"
    goto launch
)
if exist "%PROGRAMFILES%\Beyond-All-Reason\Beyond-All-Reason.exe" (
    set "BAR_EXE=%PROGRAMFILES%\Beyond-All-Reason\Beyond-All-Reason.exe"
    goto launch
)
if exist "%~dp0Beyond-All-Reason.exe" (
    set "BAR_EXE=%~dp0Beyond-All-Reason.exe"
    goto launch
)

echo.
echo Could not find Beyond-All-Reason.exe automatically.
echo Open this script and set BAR_EXE to its full path.
echo.
pause
exit /b 1

:launch
echo Routing BAR downloads through %PROXY%
echo Launching %BAR_EXE%
start "" "%BAR_EXE%" %*
endlocal
