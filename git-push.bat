@echo off

setlocal

cd /d "%~dp0"



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



echo Adding all changes...

git add .

if errorlevel 1 goto fail



git diff --cached --quiet

if errorlevel 1 (

  echo Committing...

  git commit -m "add"

  if errorlevel 1 goto fail

) else (

  echo Nothing new to commit.

)



echo Pushing...

git push

if errorlevel 1 goto fail



echo.

echo Push finished.

pause

exit /b 0



:fail

echo.

echo Failed. Read the messages above.

pause

exit /b 1

