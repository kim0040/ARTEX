#!/bin/sh
# ARTEX 감시 시작 스크립트(Linux / macOS / Docker ENTRYPOINT)
#
# 사용법:
#   ./start.sh                       포그라운드(Ctrl-C 로 중지)
#   nohup ./start.sh >artex.log 2>&1 &   백그라운드
#   ./start.sh -addr :9000           추가 인자는 artex에 그대로 전달
#
# 하는 일은 하나입니다. artex를 실행하고, 프로세스가 끝나면 종료 코드로 다시 띄울지 정합니다.
#
#   0      사용자가 정상 중지     → 루프를 나감
#   75     프로그램이 재시작 요청 → 바로 다시 실행(화면의 "한 번에 업데이트" 또는 "되돌리기")
#   그 외  비정상 종료            → 기다렸다가 다시 실행(1→2→4… 최대 60초)
#
# 다운로드, SHA256 검사, 파일 교체는 여기서 하지 않습니다. sh와 bat에 두 벌 쓰면
# 가장 틀리면 안 되는 부분이 갈라집니다. 뜨지 않는 바이너리로 갈아 끼우면 이 스크립트는
# 그 파일을 계속 다시 띄우고, 사람은 머신에 들어가 손으로 고쳐야 합니다.
# 검사와 교체는 Go의 selfupdate 패키지에 있고, artex가 켜질 때 스스로 합니다.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 실행 파일 $BIN 이 없습니다" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# 중지 신호를 artex 프로세스에 넘깁니다.
#
# Docker에서는 이것이 필요합니다. docker stop은 SIGTERM을 PID 1(이 스크립트)에만 보내고
# 자식에게는 보내지 않습니다. 넘기지 않으면 artex는 신호를 받지 못해 깔끔히 종료하지 못하고,
# 10초 뒤에 SIGKILL로 죽으며 돌아가던 작업이 중간에 끊깁니다.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# 신호는 wait를 끊고 128보다 큰 값을 돌려줍니다. 그때 자식은 아직 종료 중입니다.
	# 진짜 종료 코드를 받으려면 wait를 한 번 더 해야 합니다.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] 중지됨"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] 정상 종료"
			exit 0
			;;
		"$RESTART_CODE")
			# 업데이트나 되돌리기가 준비됨. 다시 뜨면 artex가 켜질 때 파일을 바꿉니다(selfupdate.Bootstrap).
			echo "[artex] 재시작 요청(새 버전 적용)…"
			delay=1
			;;
		*)
			echo "[artex] 비정상 종료 (code=$code), ${delay}s 후 재시작" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
