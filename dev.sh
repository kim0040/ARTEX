#!/usr/bin/env bash
# 개발 모드: 백엔드(:8787)와 기록 프록시(:8788), 프론트 next dev(:5173)를 같이 띄웁니다.
# 프론트의 /api 는 백엔드로 프록시됩니다. Ctrl-C 하면 함께 끝납니다.
#
# 화면을 바이너리에 넣는 릴리스 빌드는 README의 소스 빌드 절을 보고, 이 스크립트를 쓰지 않습니다.
set -euo pipefail
cd "$(dirname "$0")"

# 끝날 때 이 프로세스 그룹의 자식(백엔드와 프론트)을 모두 끝냅니다.
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# 백엔드. 일반 go run 이라 화면을 내장하지 않습니다. 동시 워커 수는 「시스템 설정」에서 바꿉니다.
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# 프론트 핫 리로드. /api 는 :8787 로 프록시됩니다.
( cd web && npm run dev ) &

echo "[dev] 백엔드 :8787 / 프록시 :8788 / 프론트 http://localhost:5173  (Ctrl-C 로 종료)"
wait
