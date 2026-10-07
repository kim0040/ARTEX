@echo off
rem 콘솔을 UTF-8로 바꿉니다. 그렇지 않으면 이 파일의 한글이 기본 코드 페이지에서 깨집니다.
chcp 65001 >nul 2>&1
rem ARTEX 감시 시작 스크립트(Windows)
rem
rem 사용법:
rem   start.bat                  포그라운드(Ctrl-C 로 중지)
rem   start.bat -addr :9000      추가 인자는 artex에 그대로 전달
rem
rem 하는 일은 하나입니다. artex.exe를 실행하고, 프로세스가 끝나면 종료 코드로 다시 띄울지 정합니다.
rem
rem   0      사용자가 정상 중지  -> 루프를 나감
rem   75     프로그램이 재시작 요청 -> 바로 다시 실행(화면의 "한 번에 업데이트" 또는 "되돌리기")
rem   그 외  비정상 종료         -> 기다렸다가 다시 실행(1->2->4… 최대 60초)
rem
rem 다운로드, SHA256 검사, 파일 교체는 여기서 하지 않습니다. artex가 켜질 때 스스로 합니다
rem (selfupdate 패키지). 스크립트는 단순하게 둡니다. 설명은 start.sh 맨 위와 같습니다.

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] 실행 파일 %BIN% 이 없습니다 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] 정상 종료
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem 업데이트나 되돌리기가 준비됨. 다시 뜨면 artex가 켜질 때 파일을 바꿉니다.
	echo [artex] 재시작 요청(새 버전 적용)…
	set /a delay=1
	goto loop
)

echo [artex] 비정상 종료 ^(code=!code!^), !delay!s 후 재시작 1>&2
rem timeout은 리다이렉트된 콘솔에서 실패할 수 있어 ping으로 기다립니다(N초는 N+1회).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
