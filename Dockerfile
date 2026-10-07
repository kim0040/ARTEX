# syntax=docker/dockerfile:1
#
# 실행 이미지는 이미지 안에서 컴파일하지 않고, 자주 쓰는 도구와
# **미리 컴파일한 Linux 단일 바이너리**만 넣습니다.
# 바이너리는 CI의 binaries job이 QEMU 없이 순수 Go로 교차 컴파일하고,
# 대상 아키텍처에 따라 빌드 컨텍스트의 dist/<TARGETARCH>/artex에 둡니다.
# 따라서 다중 아키텍처 빌드에서 arm64는 apt 계층만 에뮬레이션하면 되고
# Next/Go 컴파일을 다시 에뮬레이션하지 않아 훨씬 빠릅니다.
#
# 로컬에서 이미지를 직접 빌드할 때는 먼저 바이너리를 준비합니다.
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# 자주 쓰는 도구: ripgrep / curl / vim과 정찰에 쓰는 도구를 함께 설치합니다(필요에 따라 조정).
# Node는 NodeSource의 20.x를 설치합니다. bookworm 기본 apt nodejs는 18이고 Playwright는 >=20을 요구합니다.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Playwright MCP와 CLI를 전역으로 미리 설치해 실행 때 npx로 내려받지 않습니다.
# @playwright/mcp: browser MCP는 `npx @playwright/mcp`로 바로 실행합니다(전역 설치이므로 -y/@latest 불필요).
# @playwright/cli: playwright-cli를 제공하며 설치 후 --help로 실행 가능 여부를 확인합니다.
# 브라우저 관리용 playwright도 설치하고 --with-deps로 chromium과 시스템 의존성을 미리 넣습니다.
# 따라서 컨테이너 안에서 MCP/CLI를 처음 실행할 때 브라우저를 다시 내려받지 않습니다.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# 대상 아키텍처에 맞게 미리 컴파일한 바이너리(dist/amd64/artex 또는 dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# 감시 시작 스크립트: 프로세스가 끝나면 종료 코드에 따라 다시 띄우고, 화면의 한 번에 업데이트는 이 스크립트가 교체를 마칩니다.
# SIGTERM도 artex에 전달합니다. docker stop은 PID 1에만 신호를 보내므로,
# 전달하지 않으면 artex가 우아하게 종료되지 않고 10초 뒤 SIGKILL로 강제 종료됩니다.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# 라이선스와 고지 문서. 원문 경로를 보존해 이미지 사용자도 확인할 수 있게 합니다.
COPY LICENSE /app/LICENSE
COPY NOTICE.md /app/NOTICE.md
COPY web/LICENSE /app/web/LICENSE
COPY web/public/legal/ /app/legal/
# data/ (SQLite + jwt.key) 영속화 지점
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
