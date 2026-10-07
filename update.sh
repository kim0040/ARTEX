#!/usr/bin/env bash
# ARTEX 업데이트 스크립트: ① Docker(새 이미지를 받아 다시 만듦)  ② 로컬 컴파일(바이너리를 다시 빌드)
# install.sh와 짝입니다. install은 처음 설치, update는 새 버전으로 올리는 일입니다.
# DB 이전은 손으로 하지 않습니다. artex는 켜질 때마다 schema.sql을 멱등으로 다시 적용합니다
# (ADD COLUMN, CREATE INDEX IF NOT EXISTS). 다시 켜는 것이 이전입니다.
# 데이터(pgdata 볼륨, ./data, ./skills)는 그대로입니다.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── 선택: 저장소를 최신 코드로 맞춥니다. compose, 스크립트, 로컬 빌드 소스가 이것으로 갱신됩니다.
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "git 작업 사본이 아니라 git pull을 건너뜁니다"; return; }
  [ "$(ask '최신 코드를 받을까요 (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull이 빨리감기로 되지 않았습니다(로컬 변경 또는 분기). 직접 맞춘 뒤 다시 하세요. 이번은 현재 코드를 씁니다"
  fi
}

# ── ① Docker 업데이트 ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose 가 없습니다. 먼저 ./install.sh 로 설치하세요"
  [ -f .env ] || die ".env가 없습니다. 먼저 ./install.sh 로 처음 설치를 하세요"

  info "현재 한국어 소스로 이미지를 다시 만듭니다…"
  bash ./build-docker.sh
  info "한국어 학습판 컨테이너를 다시 시작합니다…"
  docker compose -f docker-compose.yml -f docker-compose.ko.yml up -d artex
  ok "업데이트 완료 → http://localhost:8787"
  info "로그: docker compose logs -f artex"
  info "옛 이미지 정리(선택): docker image prune -f"
}

# ── ② 로컬 컴파일 업데이트 ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go(>=1.26)가 없습니다: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json이 없습니다. 처음 설치라면 ./install.sh 를 쓰세요"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "프론트 정적 파일을 다시 만듭니다…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "화면이 들어 있는 바이너리를 다시 컴파일합니다…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm이 없습니다. 화면을 넣지 않은 백엔드를 컴파일합니다(프론트는 npm run dev로 따로 띄우세요)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "컴파일 완료 → ./artex"
  warn "돌고 있는 artex 프로세스를 다시 시작해야 반영됩니다(다시 켤 때 스키마를 맞춥니다)"
}

echo "=============================="
echo "  ARTEX 업데이트"
echo "  1) 현재 한국어 소스로 Docker 이미지 다시 빌드(새 이미지를 받아 다시 만듦)"
echo "  2) 로컬 업데이트(go로 다시 컴파일)"
echo "=============================="
case "$(ask '선택' 2)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "잘못된 선택" ;;
esac
