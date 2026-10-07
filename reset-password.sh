#!/usr/bin/env bash
# =============================================================================
# ARTEX 관리자 비밀번호 재설정 스크립트
#
# 로그인 사용자 이름은 ARTEX로 고정입니다. 비밀번호는 bcrypt 해시로 settings 테이블의
# auth.password_hash 키에 있습니다. 이 스크립트는 데이터베이스에 연결한 뒤 pgcrypto로
# bcrypt 해시를 만들어 그 키에 씁니다. 백엔드 로그인 검사(golang.org/x/crypto/bcrypt)와 호환됩니다.
#
# 배포는 두 가지입니다.
#   local (기본) —— 호스트에서 psql로 직접 연결합니다. 연결 정보 우선순위:
#                   명령 인자 > --dsn/$ARTEX_PG_DSN > config.json 의 database.*
#   docker       —— `docker compose exec`(또는 `docker exec`)로 postgres 컨테이너 안에서
#                   psql을 실행합니다. compose는 기본적으로 5432를 호스트에 열지 않습니다.
#
# 사용 예:
#   ./reset-password.sh                          # 로컬. config.json/환경을 읽고 새 비밀번호를 입력
#   ./reset-password.sh -p 'NewPass!'            # 로컬. 새 비밀번호를 인자로 지정
#   ./reset-password.sh --dsn postgres://u:p@h:5432/artex
#   ./reset-password.sh -H 127.0.0.1 -P 5433 -U autopentest -W pass -d artex
#   ./reset-password.sh -m docker                # docker 배포(.env 의 POSTGRES_* 를 읽음)
#   ./reset-password.sh -m docker -c postgres컨테이너이름 --exec docker
#
# 안전: 새 비밀번호는 환경 변수와 psql \getenv 로 넘깁니다(프로세스 argv에 넣지 않음).
# :'var' 로 이스케이프합니다. 데이터베이스 비밀번호는 PGPASSWORD로 넘기며 역시 argv에 없습니다.
# =============================================================================
set -euo pipefail

PASS_KEY="auth.password_hash"
BCRYPT_COST=10

MODE=""            # local | docker (비우면 자동 판정)
DSN=""
HOST="" PORT="" USER="" DBPASS="" DBNAME="" SSLMODE=""
CONFIG=""
CONTAINER=""       # docker 모드의 postgres 서비스/컨테이너 이름(기본 postgres)
EXEC_KIND=""       # compose | docker (docker 모드에서 어떤 exec를 쓸지. 비우면 자동)
NEWPASS=""
ASSUME_YES=0

die() { echo "오류: $*" >&2; exit 1; }
info() { echo "· $*" >&2; }

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

# ---- 인자 -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--mode)        MODE="${2:-}"; shift 2 ;;
    --dsn)            DSN="${2:-}"; shift 2 ;;
    -H|--host)        HOST="${2:-}"; shift 2 ;;
    -P|--port)        PORT="${2:-}"; shift 2 ;;
    -U|--user)        USER="${2:-}"; shift 2 ;;
    -W|--db-password) DBPASS="${2:-}"; shift 2 ;;
    -d|--dbname)      DBNAME="${2:-}"; shift 2 ;;
    --sslmode)        SSLMODE="${2:-}"; shift 2 ;;
    --config)         CONFIG="${2:-}"; shift 2 ;;
    -c|--container)   CONTAINER="${2:-}"; shift 2 ;;
    --exec)           EXEC_KIND="${2:-}"; shift 2 ;;
    -p|--new-password) NEWPASS="${2:-}"; shift 2 ;;
    -y|--yes)         ASSUME_YES=1; shift ;;
    -h|--help)        usage ;;
    *) die "알 수 없는 인자: $1 (-h 로 사용법)" ;;
  esac
done

# ---- config.json 의 database.* 를 읽습니다(local이고 연결을 직접 주지 않았을 때).
# python3로 파싱하고, 없으면 grep으로 돌아갑니다(config.json 필드가 한 줄에 하나씩일 때).
read_config_json() {
  local path="$1"
  [[ -f "$path" ]] || return 1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1])).get("database", {})
except Exception:
    sys.exit(1)
# dsn 한 줄 또는 필드별 둘 다 됩니다
if d.get("dsn"):
    print("DSN\t" + d["dsn"]); sys.exit(0)
for k in ("host","port","user","password","dbname","sslmode"):
    if d.get(k) is not None:
        print(k.upper() + "\t" + str(d[k]))
PY
  else
    # 최소 폴백: 키마다 grep(값은 문자열 또는 숫자)
    local k
    for k in host port user password dbname sslmode; do
      local v
      v=$(grep -oE "\"$k\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9]+)" "$path" 2>/dev/null \
            | head -1 | sed -E "s/.*:[[:space:]]*//; s/^\"//; s/\"$//") || true
      [[ -n "$v" ]] && echo -e "${k^^}\t$v"
    done
  fi
}

apply_config_fields() {
  local line key val
  while IFS=$'\t' read -r key val; do
    [[ -z "$key" ]] && continue
    case "$key" in
      DSN)      [[ -z "$DSN" ]] && DSN="$val" ;;
      HOST)     [[ -z "$HOST" ]] && HOST="$val" ;;
      PORT)     [[ -z "$PORT" ]] && PORT="$val" ;;
      USER)     [[ -z "$USER" ]] && USER="$val" ;;
      PASSWORD) [[ -z "$DBPASS" ]] && DBPASS="$val" ;;
      DBNAME)   [[ -z "$DBNAME" ]] && DBNAME="$val" ;;
      SSLMODE)  [[ -z "$SSLMODE" ]] && SSLMODE="$val" ;;
    esac
  done
}

# ---- 모드 자동 판정 ---------------------------------------------------------
if [[ -z "$MODE" ]]; then
  if [[ -n "$DSN$HOST$USER$DBNAME" || -n "${ARTEX_PG_DSN:-}" || -f "${CONFIG:-config.json}" ]]; then
    MODE="local"
  elif command -v docker >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
    MODE="docker"
  else
    MODE="local"
  fi
fi
info "배포 모드: $MODE"

# ---- 새 비밀번호 -----------------------------------------------------------
if [[ -z "$NEWPASS" ]]; then
  read -r -s -p "새 비밀번호(사용자 이름은 ARTEX로 고정): " NEWPASS; echo >&2
  [[ -n "$NEWPASS" ]] || die "비밀번호가 비어 있습니다"
  read -r -s -p "한 번 더 입력: " NEWPASS2; echo >&2
  [[ "$NEWPASS" == "$NEWPASS2" ]] || die "두 입력이 다릅니다"
fi
[[ -n "$NEWPASS" ]] || die "비밀번호가 비어 있습니다"

# 환경 변수로 비밀번호를 psql에 넘깁니다(\getenv 가 읽고, argv/ps에는 없습니다)
export ARTEX_RESET_NEWPASS="$NEWPASS"

# 데이터베이스 안에서 bcrypt를 만들어 upsert 합니다. 비밀번호는 :'newpw' 로 이스케이프됩니다.
# CREATE EXTENSION은 멱등입니다. 확장 권한이 없으면 여기서 실패합니다(아래 실패 분기 문구).
SQL=$(cat <<SQL
\\set ON_ERROR_STOP on
\\getenv newpw ARTEX_RESET_NEWPASS
CREATE EXTENSION IF NOT EXISTS pgcrypto;
INSERT INTO settings(key, value)
VALUES ('$PASS_KEY', crypt(:'newpw', gen_salt('bf', $BCRYPT_COST)))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
SQL
)

# ---- 실행 -----------------------------------------------------------------
if [[ "$MODE" == "local" ]]; then
  # 연결 정보 우선순위: 명령 인자 > --dsn/$ARTEX_PG_DSN > config.json
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    [[ -n "${ARTEX_PG_DSN:-}" ]] && DSN="$ARTEX_PG_DSN"
  fi
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    cfg="${CONFIG:-config.json}"
    if [[ -f "$cfg" ]]; then
      info "$cfg 에서 데이터베이스 설정을 읽습니다"
      apply_config_fields < <(read_config_json "$cfg")
    fi
  fi

  command -v psql >/dev/null 2>&1 || die "이 머신에 psql이 없습니다(postgresql-client를 설치하거나 -m docker를 쓰세요)"

  declare -a PSQL_ARGS=()
  if [[ -n "$DSN" ]]; then
    PSQL_ARGS=("$DSN")
    target="$DSN"
  else
    [[ -n "$USER"   ]] || die "데이터베이스 사용자(-U) 또는 유효한 config.json/DSN이 없습니다"
    [[ -n "$DBNAME" ]] || die "데이터베이스 이름(-d) 또는 유효한 config.json/DSN이 없습니다"
    HOST="${HOST:-127.0.0.1}"; PORT="${PORT:-5432}"; SSLMODE="${SSLMODE:-disable}"
    PSQL_ARGS=(-h "$HOST" -p "$PORT" -U "$USER" -d "$DBNAME")
    [[ -n "$SSLMODE" ]] && export PGSSLMODE="$SSLMODE"
    [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
    target="$USER@$HOST:$PORT/$DBNAME"
  fi

  info "대상 데이터베이스: $target"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "이 데이터베이스의 ARTEX 비밀번호를 재설정할까요? [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "취소했습니다"
  fi

  if ! printf '%s\n' "$SQL" | psql "${PSQL_ARGS[@]}" -v ON_ERROR_STOP=1 -q >/dev/null; then
    die "쓰기에 실패했습니다. pgcrypto 권한이나 확장이 없다면, 확장을 만들 수 있는 역할을 쓰거나 CREATE EXTENSION pgcrypto 를 먼저 실행하세요."
  fi

else
  # ---- docker ----
  command -v docker >/dev/null 2>&1 || die "docker가 없습니다"
  CONTAINER="${CONTAINER:-postgres}"

  # exec 방식: docker compose exec(서비스 이름)를 우선하고, 아니면 docker exec(컨테이너 이름).
  if [[ -z "$EXEC_KIND" ]]; then
    if docker compose version >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
      EXEC_KIND="compose"
    else
      EXEC_KIND="docker"
    fi
  fi

  # 컨테이너 안 psql 자격: 명령 인자, 그다음 .env의 POSTGRES_*, 없으면 compose 기본값(artex).
  if [[ -f .env ]]; then
    # shellcheck disable=SC1091
    set -a; . ./.env; set +a
  fi
  DUSER="${USER:-${POSTGRES_USER:-artex}}"
  DNAME="${DBNAME:-${POSTGRES_DB:-artex}}"
  [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
  [[ -z "${PGPASSWORD:-}" && -n "${POSTGRES_PASSWORD:-}" ]] && export PGPASSWORD="$POSTGRES_PASSWORD"

  info "대상: 컨테이너 $CONTAINER 안의 psql -U $DUSER -d $DNAME (exec=$EXEC_KIND)"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "이 컨테이너 데이터베이스의 ARTEX 비밀번호를 재설정할까요? [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "취소했습니다"
  fi

  # -e 는 이름만 넘기고 값은 현재 환경에서 상속합니다. 비밀번호는 docker argv에 없습니다.
  declare -a EXEC_CMD
  if [[ "$EXEC_KIND" == "compose" ]]; then
    EXEC_CMD=(docker compose exec -T -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  else
    EXEC_CMD=(docker exec -i -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  fi

  if ! printf '%s\n' "$SQL" | "${EXEC_CMD[@]}" >/dev/null; then
    die "쓰기에 실패했습니다. 컨테이너 이름(-c), 데이터베이스 계정(.env의 POSTGRES_*), pgcrypto 권한을 확인하세요."
  fi
fi

unset ARTEX_RESET_NEWPASS
echo "✓ ARTEX 관리자 비밀번호를 재설정했습니다. 사용자 이름 ARTEX와 새 비밀번호로 로그인하세요(서비스 재시작은 필요 없습니다)."
