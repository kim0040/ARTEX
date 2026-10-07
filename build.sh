#!/usr/bin/env bash
# ARTEX 크로스 플랫폼 릴리스 빌더.
#
# 기본 모드는 하나의 대상을 빌드하고 이미 내보낸 프론트엔드를 넣습니다.
# `./build.sh --release`는 지원하는 모든 데스크톱/서버 대상을 빌드하고 묶습니다.
#
# 환경 변수:
#   ARTEX_TARGET_OS=linux             단일 대상 모드에서 빌드할 운영체제.
#   ARTEX_TARGET_ARCH=amd64           단일 대상 모드에서 빌드할 아키텍처.
#   ARTEX_TARGETS=linux/amd64,...     여러 대상을 쉼표로 구분한 목록.
#   ARTEX_BUILD_VERSION=v0.3.3        바이너리와 압축 파일 이름에 넣을 버전.
#   ARTEX_OUTPUT=/path/to/artex       단일 대상 모드에서 사용할 바이너리 경로.
#   ARTEX_OUTPUT_DIR=dist             기본 바이너리 경로의 상위 디렉터리.
#   ARTEX_PACKAGE=1                   대상별 zip 압축 파일을 만듦.
#   ARTEX_PACKAGE_DIR=dist            릴리스 압축 파일을 둘 디렉터리.
#   ARTEX_COMPRESS=off                UPX 모드: off, auto, required.
#   ARTEX_UPX_ARGS="--best --lzma"    UPX에 넘길 인자.
#   ARTEX_SKIP_FRONTEND=1             server/webui/dist를 재사용함(CI 산출물 빌드용).
#   ARTEX_SKIP_NPM_CI=1               프론트엔드를 다시 빌드할 때 npm ci를 건너뜀.
#   ARTEX_GOSUMDB=sum.golang.org      Go 체크섬 데이터베이스.
set -euo pipefail

cd "$(cd "$(dirname "$0")" && pwd)"

info() { printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok() { printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[!]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
사용법:
  ./build.sh                         현재 시스템과 아키텍처를 컴파일
  ./build.sh --target linux/amd64   지정한 대상 하나를 컴파일
  ./build.sh --release               지원하는 대상을 모두 컴파일하고 묶음

옵션:
  --release              Linux, macOS, Windows의 amd64/arm64 대상을 빌드하고 zip을 만듦
  --target OS/ARCH       대상 하나를 지정. 예: windows/amd64
  --upx                  UPX로 바이너리를 압축(일부 리눅스 환경과 안 맞을 수 있음)
  --no-compress          UPX를 쓰지 않음. Go 링커로 줄이고 zip만 압축
  --help                 도움말

여러 대상은 ARTEX_TARGETS로 덮어씁니다. 예:
  ARTEX_TARGETS=linux/amd64,windows/amd64 ./build.sh --release
EOF
}

RELEASE_TARGETS_DEFAULT="linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64"
ARTEX_RELEASE="${ARTEX_RELEASE:-0}"
ARTEX_COMPRESS="${ARTEX_COMPRESS:-off}"
ARTEX_PACKAGE="${ARTEX_PACKAGE:-0}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --release)
      ARTEX_RELEASE=1
      ARTEX_PACKAGE=1
      shift
      ;;
    --target)
      [ "$#" -ge 2 ] || die "--target 에는 OS/ARCH 인자가 필요합니다"
      target_arg="$2"
      case "$target_arg" in
        */*)
          ARTEX_TARGET_OS="${target_arg%%/*}"
          ARTEX_TARGET_ARCH="${target_arg##*/}"
          ARTEX_TARGETS="$target_arg"
          ;;
        *) die "대상은 OS/ARCH 여야 합니다. 예: linux/amd64" ;;
      esac
      shift 2
      ;;
    --no-compress)
      ARTEX_COMPRESS=0
      shift
      ;;
    --upx)
      ARTEX_COMPRESS=required
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *) die "알 수 없는 인자: $1 (--help 로 사용법)" ;;
  esac
done

command -v go >/dev/null 2>&1 || die "Go가 없습니다(이 프로젝트는 Go 1.26 이상이 필요합니다)"

ARTEX_GOSUMDB="${ARTEX_GOSUMDB:-sum.golang.org}"
if [ -z "${ARTEX_BUILD_VERSION:-}" ]; then
  if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    ARTEX_BUILD_VERSION="$(git describe --tags --always --dirty)"
  else
    ARTEX_BUILD_VERSION="dev"
  fi
fi
# 릴리스 태그는 보통 v0.3.3 형태로 전달되므로 바이너리 버전은 접두사를 빼고 맞춥니다.
ARTEX_BUILD_VERSION="${ARTEX_BUILD_VERSION#v}"
ARTEX_OUTPUT_DIR="${ARTEX_OUTPUT_DIR:-dist}"
ARTEX_PACKAGE_DIR="${ARTEX_PACKAGE_DIR:-$ARTEX_OUTPUT_DIR}"
ARTEX_UPX_ARGS="${ARTEX_UPX_ARGS:---best --lzma}"

if [ "${ARTEX_RELEASE}" = "1" ]; then
  ARTEX_TARGETS="${ARTEX_TARGETS:-$RELEASE_TARGETS_DEFAULT}"
else
  ARTEX_TARGET_OS="${ARTEX_TARGET_OS:-$(GOSUMDB="$ARTEX_GOSUMDB" go env GOOS)}"
  ARTEX_TARGET_ARCH="${ARTEX_TARGET_ARCH:-$(GOSUMDB="$ARTEX_GOSUMDB" go env GOARCH)}"
  ARTEX_TARGETS="${ARTEX_TARGETS:-${ARTEX_TARGET_OS}/${ARTEX_TARGET_ARCH}}"
fi

if [ "${ARTEX_SKIP_FRONTEND:-0}" = "1" ]; then
  [ -d server/webui/dist ] || die "ARTEX_SKIP_FRONTEND=1 인데 server/webui/dist 가 없습니다"
else
  command -v npm >/dev/null 2>&1 || die "npm이 없습니다(프론트 정적 빌드에 Node.js/npm이 필요합니다)"
  command -v rsync >/dev/null 2>&1 || die "rsync가 없습니다"
  info "프론트 정적 파일을 빌드합니다"
  if [ "${ARTEX_SKIP_NPM_CI:-0}" != "1" ]; then
    (cd web && npm ci)
  fi
  (cd web && npm run build:static)
  info "프론트 파일을 server/webui/dist 로 동기화합니다"
  mkdir -p server/webui/dist
  rsync -a --delete web/out/ server/webui/dist/
fi

compress_binary() {
  binary="$1"
  goos="$2"
  case "$ARTEX_COMPRESS" in
    0|off|false|none)
      info "UPX를 건너뜁니다: $binary"
      return 0
      ;;
    auto|required|true|1) ;;
    *) die "ARTEX_COMPRESS는 off, auto, required 중 하나여야 합니다" ;;
  esac

  if ! command -v upx >/dev/null 2>&1; then
    if [ "$ARTEX_COMPRESS" = "required" ]; then
      die "ARTEX_COMPRESS=required 인데 upx가 없습니다"
    fi
    warn "upx가 없습니다. 링커로 줄인 결과를 유지합니다: $binary"
    return 0
  fi

  before=$(wc -c < "$binary" | tr -d ' ')
  upx_args="$ARTEX_UPX_ARGS"
  [ "$goos" = "darwin" ] && upx_args="$upx_args --force-macos"
  # shellcheck disable=SC2086
  if ! upx $upx_args -- "$binary"; then
    if [ "$ARTEX_COMPRESS" = "required" ]; then
      die "UPX 압축 실패: $binary"
    fi
    warn "UPX가 이 형식을 지원하지 않습니다. 압축하지 않은 바이너리를 유지합니다: $binary"
    return 0
  fi
  after=$(wc -c < "$binary" | tr -d ' ')
  ok "UPX 압축 완료: $binary (${before} -> ${after} bytes)"
}

write_package_documents() {
  local package_root="$1"
  [ -f LICENSE ] || die "패키지에 넣을 LICENSE가 없습니다"
  [ -f web/LICENSE ] || die "패키지에 넣을 web/LICENSE가 없습니다"

  cp LICENSE "$package_root/LICENSE"

  if [ -f web/public/legal/DEPENDENCY-NOTICES.txt ]; then
    cp web/public/legal/DEPENDENCY-NOTICES.txt "$package_root/DEPENDENCY-NOTICES.txt"
  fi
  if [ -f scripts/package-source.py ]; then
    command -v python3 >/dev/null 2>&1 || die "대응 소스 묶음 생성에 Python 3가 필요합니다"
    python3 scripts/package-source.py "$package_root/SOURCE.tar.gz"
  fi

  # 두 라이선스 원문은 구분선만 덧붙여 하나의 안내 파일에 그대로 담습니다.
  {
    printf '%s\n\n' '===== LICENSE ====='
    cat LICENSE
    printf '\n\n%s\n\n' '===== web/LICENSE ====='
    cat web/LICENSE
  } > "$package_root/THIRD-PARTY-LICENSE.txt"

  # NOTICE와 안내 문서는 아직 없는 체크아웃에서도 빌드할 수 있게 선택적으로 넣습니다.
  local doc
  for doc in NOTICE.md docs/초보자-길잡이.md docs/포크-라이선스-안내.md docs/한글화-검증.md; do
    if [ -f "$doc" ]; then
      case "$doc" in
        docs/*) mkdir -p "$package_root/docs" ;;
      esac
      cp "$doc" "$package_root/$doc"
    fi
  done
}

package_binary() {
  binary="$1"
  goos="$2"
  goarch="$3"
  package_name="artex-${ARTEX_BUILD_VERSION}-${goos}-${goarch}"
  package_root="${ARTEX_PACKAGE_DIR}/${package_name}"
  archive="${ARTEX_PACKAGE_DIR}/${package_name}.zip"

  command -v python3 >/dev/null 2>&1 || die "한글 파일명을 보존해 묶으려면 Python 3가 필요합니다"
  rm -rf "$package_root" "$archive"
  mkdir -p "$package_root"
  cp "$binary" "$package_root/"
  # 감시 시작 스크립트가 정식 입구입니다. 화면의 한 번에 업데이트는
  # 프로세스가 끝난 뒤 이 스크립트가 다시 띄워야 파일 교체가 끝납니다.
  # artex만 직접 실행하면 업데이트 뒤에 다시 뜨지 않습니다. 대상 OS에 맞는 스크립트만 넣습니다.
  if [ "$goos" = "windows" ]; then
    cp start.bat "$package_root/"
  else
    cp start.sh "$package_root/"
    chmod +x "$package_root/start.sh"
  fi
  cp -R skills "$package_root/"
  cp config.example.json "$package_root/"
  if [ -f README.md ]; then cp README.md "$package_root/"; fi
  write_package_documents "$package_root"
  python3 scripts/package-zip.py "$package_root" "$archive"
  rm -rf "$package_root"
  ok "릴리스 압축 파일: $archive"
}

build_target() {
  target="$1"
  case "$target" in
    */*) ;;
    *) die "잘못된 대상: $target (OS/ARCH 여야 합니다)" ;;
  esac
  goos="${target%%/*}"
  goarch="${target##*/}"
  case "$goos" in
    linux|darwin|windows) ;;
    *) die "지원하지 않는 시스템: $goos (linux, darwin, windows)" ;;
  esac

  binary_name="artex"
  [ "$goos" = "windows" ] && binary_name="artex.exe"
  if [ -n "${ARTEX_OUTPUT:-}" ] && [ "$ARTEX_RELEASE" != "1" ]; then
    output="$ARTEX_OUTPUT"
  else
    output="${ARTEX_OUTPUT_DIR}/artex-${goos}-${goarch}/${binary_name}"
  fi
  mkdir -p "$(dirname "$output")"

  info "컴파일 ${goos}/${goarch}, 버전 ${ARTEX_BUILD_VERSION}"
  GOSUMDB="$ARTEX_GOSUMDB" \
  CGO_ENABLED=0 \
  GOOS="$goos" \
  GOARCH="$goarch" \
  go build \
    -tags embedui \
    -trimpath \
    -ldflags "-s -w -buildid= -X main.version=${ARTEX_BUILD_VERSION}" \
    -o "$output" \
    ./cmd/artex

  compress_binary "$output" "$goos"
  if command -v file >/dev/null 2>&1; then file "$output"; fi
  if [ "$ARTEX_PACKAGE" = "1" ]; then package_binary "$output" "$goos" "$goarch"; fi
  ok "컴파일 완료: $output"
}

write_checksums() {
  [ "$ARTEX_PACKAGE" = "1" ] || return 0
  checksum_file="$ARTEX_PACKAGE_DIR/SHA256SUMS"
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$ARTEX_PACKAGE_DIR" && for archive in *.zip; do sha256sum "$archive"; done > "$(basename "$checksum_file")")
  elif command -v shasum >/dev/null 2>&1; then
    (cd "$ARTEX_PACKAGE_DIR" && for archive in *.zip; do shasum -a 256 "$archive"; done > "$(basename "$checksum_file")")
  else
    warn "sha256sum 또는 shasum이 없습니다. SHA256SUMS를 건너뜁니다"
    return 0
  fi
  ok "검사 파일: $checksum_file"
}

mkdir -p "$ARTEX_OUTPUT_DIR"
if [ "$ARTEX_PACKAGE" = "1" ]; then mkdir -p "$ARTEX_PACKAGE_DIR"; fi

old_ifs="$IFS"
IFS=','
read -r -a targets <<< "$ARTEX_TARGETS"
IFS="$old_ifs"
[ "${#targets[@]}" -gt 0 ] || die "ARTEX_TARGETS가 비어 있으면 안 됩니다"
for target in "${targets[@]}"; do
  target="${target//[[:space:]]/}"
  [ -n "$target" ] || continue
  build_target "$target"
done

if [ "$ARTEX_PACKAGE" = "1" ]; then
  write_checksums
  info "릴리스 묶음 위치: $ARTEX_PACKAGE_DIR"
fi
