#!/usr/bin/env bash
# 현재 한국어 소스의 화면과 Linux 바이너리를 만든 뒤 로컬 이미지를 빌드합니다.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"
for dependency in docker go npm; do
  command -v "$dependency" >/dev/null 2>&1 || { echo "$dependency 설치가 필요합니다." >&2; exit 1; }
done
architecture="$(docker info --format '{{.Architecture}}')"
case "$architecture" in
  x86_64|amd64) target_arch=amd64 ;;
  aarch64|arm64) target_arch=arm64 ;;
  *) echo "지원하지 않는 Docker 아키텍처: $architecture" >&2; exit 1 ;;
esac
ARTEX_OUTPUT="dist/$target_arch/artex" ./build.sh --target "linux/$target_arch" --no-compress
docker compose -f docker-compose.yml -f docker-compose.ko.yml build artex
printf '%s\n' '한국어 학습판 이미지 artex-ko:local을 만들었습니다.'
