// Package config 는 JSON 설정 파일을 읽고, 환경 변수가 있으면 그 값을 우선합니다.
//
// 초보: 지금 이 파일이 주로 들고 있는 것은 PostgreSQL 접속 정보입니다.
// 자산 그래프와 탐색 그래프는 그 데이터베이스에 있습니다.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Database 는 PostgreSQL 접속 설정입니다. DSN 을 직접 넣거나, 아래 칸을 채우면
// DSN 을 조립합니다. 자산 그래프와 탐색 그래프는 이 데이터베이스에 있습니다.
type Database struct {
	DSN      string `json:"dsn"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbname"`
	SSLMode  string `json:"sslmode"`
}

// Config 는 디스크의 설정 파일 모양입니다.
type Config struct {
	Database Database `json:"database"`
	SkillDir string   `json:"skill_dir"`
}

// BaseDir 는 실행에 필요한 파일(config.json 과 data/)이 놓이는 기준 디렉터리입니다.
// 배포된 실행 파일이 있는 폴더라서, 어느 디렉터리에서 실행해도 Windows·Linux 에서
// 파일이 실행 파일 옆에 남습니다.
//
// `go run` 으로 띄우면 바이너리는 일회용입니다. 임시 빌드 폴더에 있거나 Go 빌드
// 캐시 안에 있습니다. 둘 다 알아채고 현재 작업 디렉터리로 돌아갑니다. 그래서
// 개발 중 data/ 와 설정은 프로젝트 폴더를 기준으로 찾습니다.
func BaseDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	if isGoRunDir(dir) {
		return "." // 일회용 `go run` 바이너리이면 현재 작업 디렉터리를 쓴다
	}
	return dir
}

// isGoRunDir 는 dir 이 `go run` 이 실행 파일을 둔 곳인지 봅니다. 시스템 임시
// 폴더(캐시 미스)이거나 Go 빌드 캐시(…/go-build/…) 안입니다. 캐시에 맞으면
// os.TempDir() 아래가 아니라서, 임시 폴더만 보면 가끔 놓칩니다. 둘 다 일회용
// 바이너리이므로 설정과 data 는 현재 작업 디렉터리를 기준으로 찾습니다.
func isGoRunDir(dir string) bool {
	if tmp := os.TempDir(); tmp != "" {
		if rel, err := filepath.Rel(tmp, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		if seg == "go-build" {
			return true
		}
	}
	return false
}

// Path 는 설정 파일 경로를 돌려줍니다. 찾는 순서는 다음과 같습니다.
//  1. 환경 변수 ARTEX_CONFIG (명시적 지정)
//  2. 현재 작업 디렉터리의 ./config.json (프로젝트에서 실행. `go run` 이 바이너리를
//     어디에 두든 여기를 본다)
//  3. 실행 파일 옆의 config.json (배포본은 실행 파일 옆에 둔다)
//
// 있는 파일 중 첫 번째를 씁니다. 하나도 없으면 현재 작업 디렉터리 경로를 돌려줘서,
// 없다는 안내가 사용자가 기대하는 프로젝트 폴더를 가리키게 합니다.
func Path() string {
	if v := strings.TrimSpace(os.Getenv("ARTEX_CONFIG")); v != "" {
		return v
	}
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "config.json"))
	}
	candidates = append(candidates, filepath.Join(BaseDir(), "config.json"))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

// Load 는 설정 파일을 읽어 해석합니다. 파일이 없거나 읽지 못하면 빈 Config 를
// 돌려줍니다. 호출하는 쪽이 기본값으로 넘어가게 하고, 오류로 멈추지 않습니다.
func Load() Config {
	var c Config
	b, err := os.ReadFile(Path())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	return c
}

// SkillDir 는 skill 루트 디렉터리를 돌려줍니다. 우선순위는 다음과 같습니다.
//
//	환경 변수 ARTEX_SKILL_DIR  >  설정 파일(skill_dir)  >  BaseDir()/skills
//
// 디렉터리가 없으면 만듭니다. skill 본문은 업스트림 절차라 학습 번역에서 옮기지 않습니다.
func SkillDir() string {
	var d string
	if v := strings.TrimSpace(os.Getenv("ARTEX_SKILL_DIR")); v != "" {
		d = v
	} else if v := strings.TrimSpace(Load().SkillDir); v != "" {
		d = v
	} else {
		d = filepath.Join(BaseDir(), "skills")
	}
	_ = os.MkdirAll(d, 0o755)
	return d
}

// PostgresDSN 은 접속 문자열을 정합니다. 우선순위는 다음과 같습니다.
//
//	환경 변수 ARTEX_PG_DSN  >  설정 파일(database.dsn, 또는 칸을 조립한 값)
//
// 기본 접속 주소는 없습니다. 둘 다 없으면 살펴본 설정 경로를 적은 오류를 돌려줘서,
// 잘못된 기본값에 조용히 붙지 않고 시작이 드러나게 실패합니다. source 는 시작 로그에
// 적을, DSN 이 어디서 왔는지입니다.
func PostgresDSN() (dsn, source string, err error) {
	if v := strings.TrimSpace(os.Getenv("ARTEX_PG_DSN")); v != "" {
		return v, "환경 변수 ARTEX_PG_DSN", nil
	}
	db := Load().Database
	if d := strings.TrimSpace(db.DSN); d != "" {
		return d, "설정 파일 " + Path() + " (database.dsn)", nil
	}
	if db.Host != "" || db.DBName != "" || db.User != "" {
		return db.buildDSN(), "설정 파일 " + Path() + " (database 필드)", nil
	}
	return "", "", fmt.Errorf("데이터베이스 설정을 찾지 못했습니다. 환경 변수 ARTEX_PG_DSN이 없고, 설정 파일 %s에도 database(dsn 또는 host/user/dbname)가 없습니다. 설정 파일을 만들거나 환경 변수를 지정한 뒤 다시 시도하세요", Path())
}

func (d Database) buildDSN() string {
	host := d.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := d.Port
	if port == 0 {
		port = 5432
	}
	ssl := d.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	u := url.URL{
		Scheme: "postgres",
		Host:   host + ":" + strconv.Itoa(port),
		Path:   "/" + d.DBName,
	}
	if d.User != "" {
		if d.Password != "" {
			u.User = url.UserPassword(d.User, d.Password)
		} else {
			u.User = url.User(d.User)
		}
	}
	u.RawQuery = url.Values{"sslmode": {ssl}}.Encode()
	return u.String()
}

// Redact 는 로그에 찍을 접속 문자열입니다. 비밀번호는 가립니다.
func Redact(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	if u.User != nil {
		if _, hasPw := u.User.Password(); hasPw {
			u.User = url.UserPassword(u.User.Username(), "****")
		}
	}
	return fmt.Sprintf("%s", u.String())
}
