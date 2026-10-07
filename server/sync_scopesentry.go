package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/mcphttp"
)

// 자산 동기화(ScopeSentry 데이터 소스). 초보자: 받아 온 자산은 엔진이 자산 그래프에 기록하고, 탐색 그래프는 그 노드를 이후 탐색의 출발로 쓴다.
//
// ScopeSentry는 ASM 자산 측량 플랫폼이며, MCP 인터페이스로 【프로젝트】/【작업】 두 차원에 따라
// 하위 도메인, 웹 애플리케이션, 서비스 등의 자산을 가져와 ARTEX의 회사 + 자산 모델로 매핑한다. 데이터 소스 자체는
// "ScopeSentry"라는 이름의 http 전송 MCP 행이다(url + X-API-Key 헤더는 mcp_servers에 저장된다).
//
// agent 도구 층과 달리, 여기서는 mcphttp.Client.Call로 MCP 도구를 직접 호출해 원본 JSON을 받고,
// AskUser 권한 팝업을 띄우지 않는다(백그라운드 일괄 동기화).

const (
	scopeSentryMCPName = "ScopeSentry"
	syncMaxPerType     = 5000 // 단일 대상·단일 유형의 입고 보호 상한
	syncDefaultPage    = 100
)

// findMCPByName은 그 이름의 MCP 서버 행을 돌려줍니다. 없으면 nil.
func (s *Server) findMCPByName(name string) (*db.MCPServer, error) {
	all, err := s.m.pg.ListMCP()
	if err != nil {
		return nil, err
	}
	for _, m := range all {
		if m.Name == name {
			return m, nil
		}
	}
	return nil, nil
}

// scopeSentryClient는 설정된 ScopeSentry MCP에 접속합니다(http 전송). env
// 맵은 HTTP 헤더로도 씁니다(X-API-Key). 호출자가 Close해야 합니다.
func (s *Server) scopeSentryClient(ctx context.Context) (*mcphttp.Client, error) {
	m, err := s.findMCPByName(scopeSentryMCPName)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("데이터 소스 %s이(가) 없습니다. 먼저 만드세요", scopeSentryMCPName)
	}
	if m.URL == "" {
		return nil, fmt.Errorf("데이터 소스 %s에 URL이 설정되지 않았습니다. 먼저 설정하세요", scopeSentryMCPName)
	}
	return mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
}

// envHasValue는 env/헤더 맵에 비어 있지 않은 값이 있는지 알려 줍니다
// (즉 API 키나 Authorization 헤더를 채웠는지).
func envHasValue(raw json.RawMessage) bool {
	for _, v := range jsonStrMap(raw) {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// ---------- GET /api/sync/scopesentry/status — 상태 ----------

func (s *Server) syncSSStatus(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	m, err := s.findMCPByName(scopeSentryMCPName)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp := map[string]any{
		"exists":     m != nil,
		"configured": false,
		"enabled":    false,
		"reachable":  false,
		"tools":      []string{},
	}
	if m == nil {
		writeJSON(w, 200, resp)
		return
	}
	configured := m.URL != "" && envHasValue(m.Env)
	resp["configured"] = configured
	resp["enabled"] = m.Enabled
	resp["url"] = m.URL
	if m.Tools != nil {
		resp["tools"] = m.Tools
	}
	// 실제로 접속될 수 있을 때만 가벼운 도달 확인을 합니다.
	if configured && m.Enabled {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		if cl, cerr := mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure); cerr == nil {
			if _, terr := cl.Tools(ctx); terr == nil {
				resp["reachable"] = true
			}
			_ = cl.Close()
		}
	}
	writeJSON(w, 200, resp)
}

// ---------- POST /api/sync/scopesentry/datasource — 데이터 소스 ----------
// 없으면 자리 표시 행을 만들고, URL과 API 키를 넣거나 켭니다.

func (s *Server) syncSSDatasource(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		URL    string `json:"url"`
		APIKey string `json:"api_key"`
	}
	_ = decode(r, &body) // 빈 본문 = 자리 표시만 만듭니다.
	body.URL = strings.TrimSpace(body.URL)
	body.APIKey = strings.TrimSpace(body.APIKey)

	m, err := s.findMCPByName(scopeSentryMCPName)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if m == nil {
		m = &db.MCPServer{Name: scopeSentryMCPName, Transport: "http", Env: json.RawMessage(`{"X-API-Key":""}`)}
	}
	if body.URL != "" {
		m.URL = body.URL
	}
	if body.APIKey != "" {
		env, _ := json.Marshal(map[string]string{"X-API-Key": body.APIKey})
		m.Env = env
	}
	// 실제로 쓸 수 있을 때만 켭니다.
	m.Enabled = m.URL != "" && envHasValue(m.Env)

	id, err := pg.SaveMCP(m)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	m.ID = id
	// 상태 카드가 도구를 바로 보이게, 도구 찾기는 실패해도 흐름을 계속합니다.
	if m.Enabled {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		_ = s.discoverAndCacheMCP(ctx, m)
		cancel()
	}
	writeJSON(w, 200, map[string]any{"id": id, "enabled": m.Enabled})
}

// ---------- GET /api/sync/scopesentry/projects — 프로젝트 ----------

func (s *Server) syncSSProjects(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cl, err := s.scopeSentryClient(ctx)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer cl.Close()

	args := map[string]any{
		"pageIndex": queryInt(r, "page", 1),
		"pageSize":  queryInt(r, "size", 50),
	}
	if q := r.URL.Query().Get("search"); q != "" {
		args["search"] = q
	}
	text, err := cl.Call(ctx, "list_projects_data", args)
	if err != nil {
		writeErr(w, 502, "list_projects_data 실패: "+err.Error())
		return
	}
	// {result:{All:[{id,name,logo,AssetCount,tag}], <tag>:[...]}, tag:{...}} 모양
	var env struct {
		Result map[string]json.RawMessage `json:"result"`
		Tag    map[string]int             `json:"tag"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		writeErr(w, 502, "프로젝트 목록 파싱 실패: "+err.Error())
		return
	}
	projects := json.RawMessage("[]")
	if raw, ok := env.Result["All"]; ok && len(raw) > 0 {
		projects = raw
	}
	writeJSON(w, 200, map[string]any{"projects": projects, "tag": env.Tag})
}

// ---------- GET /api/sync/scopesentry/tasks — 작업 목록 ----------

func (s *Server) syncSSTasks(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cl, err := s.scopeSentryClient(ctx)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer cl.Close()

	args := map[string]any{
		"pageIndex": queryInt(r, "page", 1),
		"pageSize":  queryInt(r, "size", 50),
	}
	if q := r.URL.Query().Get("search"); q != "" {
		args["search"] = q
	}
	text, err := cl.Call(ctx, "list_tasks", args)
	if err != nil {
		writeErr(w, 502, "list_tasks 실패: "+err.Error())
		return
	}
	var env struct {
		List json.RawMessage `json:"list"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		writeErr(w, 502, "작업 목록 파싱 실패: "+err.Error())
		return
	}
	tasks := env.List
	if len(tasks) == 0 {
		tasks = json.RawMessage("[]")
	}
	writeJSON(w, 200, map[string]any{"tasks": tasks})
}

// ---------- POST /api/sync/scopesentry/sync — 동기화 ----------

type ssSyncReq struct {
	Dimension     string   `json:"dimension"`   // "project"=프로젝트 | "task"=작업
	Targets       []string `json:"targets"`     // 프로젝트 ObjectID, 또는 작업 이름
	AssetTypes    []string `json:"asset_types"` // subdomain=서브도메인 | app=앱 | service=서비스
	CreateCompany bool     `json:"create_company"`
	PageSize      int      `json:"page_size"`
}

func (s *Server) syncSSRun(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	as := s.assetStore()
	cs := s.companyStore()
	if as == nil || cs == nil {
		writeErr(w, 503, "데이터베이스를 쓸 수 없습니다")
		return
	}
	var req ssSyncReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "JSON 형식이 올바르지 않습니다: "+err.Error())
		return
	}
	if req.Dimension != "project" && req.Dimension != "task" {
		writeErr(w, 400, "dimension은 project 또는 task여야 합니다")
		return
	}
	if len(req.Targets) == 0 {
		writeErr(w, 400, "targets는 비어 있을 수 없습니다")
		return
	}
	if len(req.AssetTypes) == 0 {
		req.AssetTypes = []string{"subdomain", "service", "app"}
	}
	pageSize := req.PageSize
	if pageSize <= 0 || pageSize > 500 {
		pageSize = syncDefaultPage
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	cl, err := s.scopeSentryClient(ctx)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer cl.Close()

	synced := map[string]int{"subdomain": 0, "app": 0, "service": 0, "ip": 0}
	var companies []string
	var warnings, errs []string
	madeCompany := false

	for _, target := range req.Targets {
		filter := map[string]any{}
		switch req.Dimension {
		case "project":
			filter["project"] = []string{target}
			if req.CreateCompany {
				name, roots, perr := s.ssProjectMeta(ctx, cl, target)
				if perr != nil {
					warnings = append(warnings, fmt.Sprintf("프로젝트 %s 상세 조회 실패: %v", target, perr))
				} else if name != "" {
					cid, cerr := cs.UpsertByName(name)
					if cerr != nil {
						warnings = append(warnings, fmt.Sprintf("회사 %s 생성 실패: %v", name, cerr))
					} else {
						companies = append(companies, name)
						madeCompany = true
						if len(roots) > 0 {
							cs.AddScope(cid, roots, "scopesentry:"+name)
						}
					}
				}
			}
		case "task":
			filter["task"] = []string{target}
		}

		for _, at := range req.AssetTypes {
			ssType, ok := map[string]string{"subdomain": "subdomain", "service": "asset", "app": "app"}[at]
			if !ok {
				warnings = append(warnings, "알 수 없는 자산 유형, 건너뜀: "+at)
				continue
			}
			items, truncated, ferr := s.ssPageAll(ctx, cl, ssType, filter, pageSize)
			if ferr != nil {
				errs = append(errs, fmt.Sprintf("%s(%s) 가져오기 실패: %v", at, target, ferr))
				continue
			}
			if truncated {
				warnings = append(warnings, fmt.Sprintf("%s(%s)이(가) %d건 상한에 도달하여 잘림", at, target, syncMaxPerType))
			}
			for _, raw := range items {
				if e := s.ssIngest(as, at, raw, synced); e != "" {
					errs = append(errs, e)
				}
			}
		}
	}

	// 기업 귀속을 다시 만들어, 방금 동기화한 자산이 기업에 붙게 합니다
	// (범위는 위에서 썼고, 자산은 그 뒤에 왔습니다).
	if madeCompany {
		_ = cs.RecomputeAttribution()
	}

	writeJSON(w, 200, map[string]any{
		"synced":    synced,
		"companies": companies,
		"warnings":  warnings,
		"errors":    errs,
	})
}

// ssProjectMeta는 프로젝트 표시 이름과 대상 루트 도메인을 가져옵니다
// (get_project.target은 줄바꿈으로 나뉜 목록). 기업과 범위를 만들 때 씁니다.
func (s *Server) ssProjectMeta(ctx context.Context, cl *mcphttp.Client, projectID string) (name string, roots []string, err error) {
	text, err := cl.Call(ctx, "get_project", map[string]any{"id": projectID})
	if err != nil {
		return "", nil, err
	}
	var p struct {
		Name   string `json:"name"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		return "", nil, err
	}
	for _, line := range strings.Split(p.Target, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			roots = append(roots, t)
		}
	}
	return p.Name, roots, nil
}

// ssPageAll은 자산 종류와 필터 하나로 list_assets를 끝까지 넘깁니다.
// 짧은/빈 페이지나 보호 상한에서 멈춥니다. {list:[...]} 봉투에 총계가 없어,
// 페이지가 pageSize보다 적게 오면 멈춥니다.
func (s *Server) ssPageAll(ctx context.Context, cl *mcphttp.Client, ssType string, filter map[string]any, pageSize int) (items []json.RawMessage, truncated bool, err error) {
	for page := 1; ; page++ {
		args := map[string]any{"asset_type": ssType, "pageIndex": page, "pageSize": pageSize}
		if len(filter) > 0 {
			args["filter"] = filter
		}
		text, cerr := cl.Call(ctx, "list_assets", args)
		if cerr != nil {
			return items, truncated, cerr
		}
		var env struct {
			List []json.RawMessage `json:"list"`
		}
		if uerr := json.Unmarshal([]byte(text), &env); uerr != nil {
			return items, truncated, uerr
		}
		if len(env.List) == 0 {
			break
		}
		items = append(items, env.List...)
		if len(items) >= syncMaxPerType {
			items = items[:syncMaxPerType]
			truncated = true
			break
		}
		if len(env.List) < pageSize {
			break
		}
	}
	return items, truncated, nil
}

// ssIngest는 ScopeSentry 자산 JSON 하나를 ARTEX 자산 저장소에 맞춰 넣고 업서트합니다.
// 실패하면 비어 있지 않은 오류 문자열을 돌려줍니다. synced는 종류마다 올라갑니다.
// 초보용: 바깥에서 온 자산을 자산 그래프에 넣습니다. 탐색 그래프의 앵커와는 별개입니다.
func (s *Server) ssIngest(as *db.AssetStore, assetType string, raw json.RawMessage, synced map[string]int) string {
	switch assetType {
	case "subdomain":
		var it struct {
			Host  string   `json:"host"`
			Type  string   `json:"type"`
			Value []string `json:"value"`
			IP    []string `json:"ip"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			return "subdomain 파싱 실패: " + err.Error()
		}
		if it.Host == "" {
			return ""
		}
		if _, err := as.UpsertSubdomain(db.UpsertSubdomainReq{Domain: it.Host, RecordType: it.Type, RecordValue: it.Value}); err != nil {
			return "subdomain " + it.Host + ": " + err.Error()
		}
		synced["subdomain"]++
		for _, ip := range it.IP {
			if ip != "" {
				if _, err := as.UpsertIP(db.UpsertIPReq{IP: ip, BoundDomains: []string{it.Host}}); err == nil {
					synced["ip"]++
				}
			}
		}
	case "app":
		var it struct {
			Name        string `json:"name"`
			Category    string `json:"category"`
			Description string `json:"description"`
			ICP         string `json:"icp"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			return "app 파싱 실패: " + err.Error()
		}
		if it.Name == "" {
			return ""
		}
		if _, err := as.UpsertApp(db.UpsertAppReq{Name: it.Name, Category: it.Category, Description: it.Description, ICP: it.ICP}); err != nil {
			return "app " + it.Name + ": " + err.Error()
		}
		synced["app"]++
	case "service":
		var it struct {
			Domain   string   `json:"domain"`
			IP       string   `json:"ip"`
			Port     string   `json:"port"`
			Service  string   `json:"service"`
			URL      string   `json:"url"`
			Title    string   `json:"title"`
			Status   *int     `json:"status"`
			Products []string `json:"products"`
			Icon     string   `json:"icon"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			return "service 파싱 실패: " + err.Error()
		}
		if it.Service == "http" || it.URL != "" {
			if it.URL == "" {
				return ""
			}
			if _, err := as.UpsertHTTPService(db.UpsertHTTPServiceReq{
				URL: it.URL, Technologies: it.Products, StatusCode: it.Status,
				PageTitle: it.Title, FaviconMMH3: it.Icon, IP: it.IP,
			}); err != nil {
				return "service " + it.URL + ": " + err.Error()
			}
		} else {
			port, _ := strconv.Atoi(it.Port)
			if _, err := as.UpsertOtherService(db.UpsertOtherServiceReq{
				Domain: it.Domain, IP: it.IP, Port: port, ServiceName: it.Service,
			}); err != nil {
				return "service " + it.IP + ":" + it.Port + ": " + err.Error()
			}
		}
		synced["service"]++
	}
	return ""
}

// queryInt는 정수 쿼리를 읽습니다. 없으면 기본값을 씁니다.
func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
