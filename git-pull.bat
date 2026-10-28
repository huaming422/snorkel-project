@echo off

setlocal

cd /d "%~dp0snorkel-project"



where git >nul 2>&1

if errorlevel 1 (

  if exist "C:\Program Files\Git\cmd\git.exe" (

    set "PATH=C:\Program Files\Git\cmd;%PATH%"

  ) else if exist "%LOCALAPPDATA%\Programs\Git\cmd\git.exe" (

    set "PATH=%LOCALAPPDATA%\Programs\Git\cmd;%PATH%"

  ) else (

    echo Git was not found. Install Git for Windows, then try again.

    pause

    exit /b 1

  )

)



echo Pulling latest changes...

git pull

echo.

if errorlevel 1 (

  echo Pull failed.

) else (

  echo Pull finished.

)

pause

