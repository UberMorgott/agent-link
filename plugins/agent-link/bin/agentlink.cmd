@echo off
rem Copyright (c) 2026 Morgott
rem Runs agentlink with this script's arguments; stdin, stdout, stderr and the
rem exit code pass through. The executable is, in order: %AGENTLINK_EXE%, the
rem path the desktop app records at start in %APPDATA%\agentlink\executable.path,
rem agentlink.exe on PATH. An agentlink.exe in this script's own folder is never
rem run: the plugin ships none, so one there is a leftover of an older version
rem that a plugin install copied along (issue #31). cmd.exe built-ins and
rem where.exe only: no interpreter to install.
setlocal
if defined AGENTLINK_EXE (
  if exist "%AGENTLINK_EXE%" (
    set "AL_EXE=%AGENTLINK_EXE%"
    goto run
  )
  >&2 echo agentlink: AGENTLINK_EXE is set but "%AGENTLINK_EXE%" does not exist: fix or unset it.
  exit /b 1
)
set "AL_EXE="
if exist "%APPDATA%\agentlink\executable.path" set /p AL_EXE=<"%APPDATA%\agentlink\executable.path"
if defined AL_EXE if exist "%AL_EXE%" goto run
set "AL_EXE="
for /f "delims=" %%I in ('"%SystemRoot%\System32\where.exe" $PATH:agentlink.exe 2^>nul') do if /i not "%%~dpI"=="%~dp0" (
  set "AL_EXE=%%I"
  goto run
)
>&2 echo agentlink: agentlink.exe not found. Start the agentlink desktop app once ^(it records its path in %%APPDATA%%\agentlink\executable.path^), put agentlink.exe on PATH, or set AGENTLINK_EXE to its full path. Download: https://github.com/UberMorgott/agent-link/releases
exit /b 1
:run
"%AL_EXE%" %*
exit /b %ERRORLEVEL%