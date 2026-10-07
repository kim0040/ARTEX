package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// assetInterceptCandidates 는 넣으려는 자산 입력 한 건에서 도메인/IP/URL 후보 문자열을 뽑습니다. 자산 가로채기 매칭에 씁니다.
// URL 의 host 를 갈라 분류해서, URL 만 있는 서비스/엔드포인트 자산도 도메인/IP 규칙에 걸릴 수 있습니다.
func assetInterceptCandidates(item assetInputItem) (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, item.Domain)
	for _, d := range item.BoundDomains {
		add(&domains, d)
	}
	add(&ips, item.IP)
	add(&ips, item.ServiceIP)
	add(&urls, item.URL)
	if item.URL != "" {
		if u, err := url.Parse(item.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// assetInputLabel 은 넣으려는 자산의 짧은 표시 이름을 돌려줍니다. 가로채기 설명에 씁니다.
func assetInputLabel(item assetInputItem) string {
	typ := strings.TrimSpace(item.Type)
	var target string
	switch {
	case strings.TrimSpace(item.Domain) != "":
		target = strings.TrimSpace(item.Domain)
	case strings.TrimSpace(item.URL) != "":
		target = strings.TrimSpace(item.URL)
	case strings.TrimSpace(item.IP) != "":
		target = strings.TrimSpace(item.IP)
	case strings.TrimSpace(item.ServiceIP) != "":
		target = strings.TrimSpace(item.ServiceIP)
	default:
		target = "(未知)" // han-allow 업스트림 프롬프트·픽스처
	}
	if typ != "" {
		return fmt.Sprintf("[%s] %s", typ, target)
	}
	return target
}

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"批量登记新发现的资产，一次可混合多种类型（type 见枚举）。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"各类型必填字段：root_domain→domain；ip→ip（须为 IPv4/IPv6，非主机名）；subdomain→domain；app→app_name；service(HTTP)→url；service(非HTTP)→service_name+port（ip/domain 至少填一个）；endpoint→url+method。其余字段含义见各自说明。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"auth/technologies/params 为追加合并(append)，不覆盖原值。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"返回：{results:[{index,id,type}], errors:[{index,error}]}", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			// task_id 는 모델에 노출하지 않습니다. 워커가 어느 작업에 속하는지는 프로그램이 SetTaskID 로 확정합니다(handler 참고).
			"assets": map[string]any{
				"type":        "array",
				"description": "资产数组，每个元素对应一条资产记录", // han-allow 업스트림 프롬프트·픽스처
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "资产类型", // han-allow 업스트림 프롬프트·픽스처
					},
					// root_domain / subdomain
					"domain":      str("根域名或子域名（root_domain/subdomain 必填）"),        // han-allow 업스트림 프롬프트·픽스처
					"icp":         str("ICP 备案号（可选）"),                              // han-allow 업스트림 프롬프트·픽스처
					"record_type": str("DNS 解析类型：A/AAAA/CNAME/MX 等（subdomain 可选）"), // han-allow 업스트림 프롬프트·픽스처
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS 解析值列表（subdomain 可选，如 [\"1.2.3.4\",\"2.3.4.5\"]）", // han-allow 업스트림 프롬프트·픽스처
					},
					// ip
					"ip": str("IP 地址，必须是 IPv4/IPv6 地址，不能填主机名（主机名请用 type=subdomain 的 domain 字段）；ip 类型必填；service/endpoint 类型可填，用于关联 IP"), // han-allow 업스트림 프롬프트·픽스처
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "该 IP 绑定的域名列表（ip 类型可选）", // han-allow 업스트림 프롬프트·픽스처
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "开放端口列表（ip 类型可选）", // han-allow 업스트림 프롬프트·픽스처
						"items": obj(map[string]any{
							"port":    intp("端口号"),                        // han-allow 업스트림 프롬프트·픽스처
							"service": str("服务名称，如 http/ssh/mysql 等（可选）"), // han-allow 업스트림 프롬프트·픽스처
						}, "port"),
					},
					// app
					"app_name":    str("应用名称（app 类型必填）"),                                                        // han-allow 업스트림 프롬프트·픽스처
					"bundle_id":   str("Bundle ID（app 类型可选）"),                                                   // han-allow 업스트림 프롬프트·픽스처
					"category":    str("应用分类（可选）"),                                                              // han-allow 업스트림 프롬프트·픽스처
					"description": str("应用描述（可选）"),                                                              // han-allow 업스트림 프롬프트·픽스처
					"app_icp":     str("应用 ICP 备案（可选）"),                                                         // han-allow 업스트림 프롬프트·픽스처
					"company_id":  intp("归属企业 id（app 类型可选；app 无法靠 scope 自动归因，需显式指定。id 由 add_company_scope 返回）"), // han-allow 업스트림 프롬프트·픽스처
					// service (http)
					"url":         str("完整 URL，含协议和端口（HTTP 服务必填；service_type 自动设为 http）"), // han-allow 업스트림 프롬프트·픽스처
					"status_code": intp("HTTP 响应状态码，如 200/301/403/404（可选）"),               // han-allow 업스트림 프롬프트·픽스처
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP 响应体字节数（可选）", // han-allow 업스트림 프롬프트·픽스처
					},
					"page_title":   str("页面 <title> 内容（可选）"),   // han-allow 업스트림 프롬프트·픽스처
					"favicon_mmh3": str("favicon MMH3 哈希（可选）"), // han-allow 업스트림 프롬프트·픽스처
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "指纹/技术栈列表，如 [\"Nginx\",\"Vue\",\"Bootstrap\"]（可选）", // han-allow 업스트림 프롬프트·픽스처
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "发现的认证信息列表，每条含 type/username/password 等字段（可选，追加不覆盖）", // han-allow 업스트림 프롬프트·픽스처
						"items":       map[string]any{"type": "object"},
					},
					// service (other, HTTP 가 아님)
					"service_name": str("服务名称，如 ssh/mysql/redis（service 非 HTTP 时必填）"), // han-allow 업스트림 프롬프트·픽스처
					"port":         intp("端口号（service 非 HTTP 时必填）"),                   // han-allow 업스트림 프롬프트·픽스처
					// endpoint
					"method": str("HTTP 方法：GET/POST/PUT/PATCH/DELETE 等（endpoint 必填）"), // han-allow 업스트림 프롬프트·픽스처
					"params": map[string]any{
						"type":        "array",
						"description": "请求参数列表，每条含 location(query/body/header/path)/name/value/type（可选，追加不覆盖）", // han-allow 업스트림 프롬프트·픽스처
						"items":       map[string]any{"type": "object"},
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets 가 켜져 있지 않습니다. AssetStore 가 초기화되지 않았습니다"), nil // han-allow 업스트림 프롬프트·픽스처
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id 는 프로그램이 확정합니다(워커: SetTaskID). 모델 입력은 받지 않습니다. 빠뜨리거나 잘못 넘겨
			// 자산이 작업에 안 붙거나 다른 작업에 붙는 일을 막기 위해서입니다. 작업 맥락이 없는 호출자(auto/pentest/chat)는 t.taskID=0 입니다.
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			// 자산 게이트 규칙은 한 번에 읽습니다. 읽기에 실패하면 판정을 건너뜁니다(삽입은 막지 않음).
			// 차단 규칙 = 전역 ∪ 작업 block. 허용 규칙 = 작업 allow.
			blockRules, _ := t.as.ListAssetInterceptRules()
			var allowRules []db.AssetInterceptRule
			if t.taskID > 0 {
				if tb, ta, err := t.as.TaskInterceptRulesSplit(t.taskID); err == nil {
					blockRules = append(blockRules, tb...)
					allowRules = ta
				}
			}

			for i, item := range a.Assets {
				// 자산 게이트: 먼저 차단하고 그다음 허용합니다. 거부된 자산은 넣지 않습니다(Upsert 와 이후 부작용을 건너뜀).
				domains, ips, urls := assetInterceptCandidates(item)
				if d := db.EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
					errs = append(errs, errEntry{
						Index: i,
						Error: fmt.Sprintf("资产 %s %s，已禁止插入", assetInputLabel(item), d.Reason), // han-allow 업스트림 프롬프트·픽스처
					})
					continue
				}

				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent 通过 insert_assets 登记" // han-allow 업스트림 프롬프트·픽스처
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker 意图 #%d 通过 insert_assets 登记", t.ownerNode) // han-allow 업스트림 프롬프트·픽스처
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// 테스트 범위에 자동으로 넣기(source='auto'): 워커가 최상위에서 명시적으로 넣은 이 항목만, 그
				// 유형에 맞춰 보수적인 범위를 더합니다. 부작용으로 파생된 자산은 여기를 타지 않아 범위가 함부로 넓어지지 않습니다. taskID=0 이면 아무 일도 하지 않습니다.
				// 커버리지 스위치와는 별개입니다. task_scope 는 작업의 범위 경계(목록/조회의 필터 기준)이고,
				// 커버리지 스위치는 그것을 분모로 지표를 계산할지만 정합니다. 범위 자체를 쌓을지는 정하지 않습니다.
				{
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"把域名/IP/CIDR/ICP备案/企业关键词加入某公司的【资产范围】——域名、网络和ICP会自动认领命中的资产，关键词只提供给Agent作为范围提示。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"公司名唯一：company 不存在则新建，已存在则复用(只把范围并进去)。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"scope 一行一条，系统自动识别：根域名 / URL / 单个 IP / CIDR 网段 / ICP备案 / 企业关键词。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"务必给 reason 说明归属依据(whois/证书/ASN 等)。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"护栏：拒绝裸 TLD 与过宽网段(IPv4前缀需为/16-/32、IPv6前缀需为/32-/128)，非法行会被跳过并在 errors 返回。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"company": str("公司名(不存在则新建、存在则复用；名称唯一)"),                         // han-allow 업스트림 프롬프트·픽스처
			"scope":   str("资产范围，一行一条：域名 / URL / IP / CIDR / ICP备案 / 企业关键词"), // han-allow 업스트림 프롬프트·픽스처
			"reason":  str("归属依据(证据/来源)，务必填写"),                               // han-allow 업스트림 프롬프트·픽스처
			"logo":    str("公司图标 URL(可选；仅新建公司时生效)"),                          // han-allow 업스트림 프롬프트·픽스처
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope 가 켜져 있지 않습니다. CompanyStore 가 초기화되지 않았습니다"), nil // han-allow 업스트림 프롬프트·픽스처
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("company 는 비울 수 없습니다"), nil // han-allow 업스트림 프롬프트·픽스처
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("회사를 만들거나 가져오지 못했습니다: " + err.Error()), nil // han-allow 업스트림 프롬프트·픽스처
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"把测试范围加入【本任务】——这是本任务的授权边界，也是资产测试覆盖度的分母。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"kind 支持：company(整个公司名下资产) / root_domain(整个根域，含所有子域) / subdomain(单个精确子域) / ip / cidr / icp / keyword。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"说明：worker 逐个碰到的主机会被系统【自动】加进范围(精确子域)；本工具用于【主动扩大】——把整个根域/整个公司纳入，或补充指定某子域/IP。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"value：company 传公司名或 id(公司须已存在)；root_domain/subdomain 传域名；ip/cidr 传 IP 或网段；icp/keyword 传备案号或企业关键词。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"务必给 reason 说明依据(可审计)。多条用 entries 数组。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "批量：[{kind, value}]。kind∈company/root_domain/subdomain/ip/cidr/icp/keyword。", "items": map[string]any{"type": "object"}}, // han-allow 업스트림 프롬프트·픽스처
			"kind":    str("[单条] company / root_domain / subdomain / ip / cidr / icp / keyword"),                                                                                               // han-allow 업스트림 프롬프트·픽스처
			"value":   str("[单条] 公司名或id / 域名 / IP / CIDR / ICP / 关键词"),                                                                                                                         // han-allow 업스트림 프롬프트·픽스처
			"reason":  str("加入依据(用于审计)，务必填写"),                                                                                                                                                  // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope 가 켜져 있지 않습니다. AssetStore 가 초기화되지 않았습니다"), nil // han-allow 업스트림 프롬프트·픽스처
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope 에는 작업 맥락이 필요합니다(현재 task 없음)"), nil // han-allow 업스트림 프롬프트·픽스처
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // 한 건 모드
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"查询【本任务及直接关联任务】范围内、还没被事实锚点覆盖的资产（关联范围只读，供你自己判断要不要补测，不代替你决策）。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"可选按资产类型过滤：root_domain/subdomain/service/app/endpoint/ip。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"分页：page 从 1 起、page_size 默认 10。返回 {assets:[{id,type,label}], total, page, page_size}。仅任务上下文可用。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"type":      str("资产类型过滤（可选）：root_domain/subdomain/service/app/endpoint/ip"), // han-allow 업스트림 프롬프트·픽스처
			"page":      intp("页码，从 1 起（默认 1）"),                                          // han-allow 업스트림 프롬프트·픽스처
			"page_size": intp("每页数量（默认 10）"),                                             // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets 가 켜져 있지 않습니다. AssetStore 가 초기화되지 않았습니다"), nil // han-allow 업스트림 프롬프트·픽스처
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets 에는 작업 맥락이 필요합니다"), nil // han-allow 업스트림 프롬프트·픽스처
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"查询资产库：DSL 表达式搜索，或按 id/ids 直取；支持分页。只返回【本任务及直接关联任务】测试范围内的资产。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"DSL：field=value 模糊(ILIKE) | field==value 精确 | field!=value 排除 | 数字字段支持 > >= < <= | 裸词=全文模糊；AND/OR 组合(AND 优先级高)，可用括号分组。资产类型用独立 type 参数，不写进 DSL。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"未传 id/ids 时 dsl 必须非空（不允许无条件全量查询）。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"可用字段：domain(根/子/服务域名)、root_domain、ip、url、page_title、icp、service_name、app_name、method(如 GET/POST)、service_type(http|other)、record_type(如 A/CNAME)、technology(数组，=模糊 ==精确)、port/status_code/company_id(整数)。\n"+ // han-allow 업스트림 프롬프트·픽스처
			"示例：status_code>=400 AND technology=shiro ；(port==80 OR port==443) AND technology=nginx", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"dsl":    str(`DSL 查询表达式（语法/字段见工具描述）。未传 id/ids 时必须非空。`),                                                                                // han-allow 업스트림 프롬프트·픽스처
			"type":   str("资产类型过滤：root_domain|ip|subdomain|app|service|endpoint（独立字段，可与 dsl 叠加；单独 type 不足以查询，仍需 dsl）"),                             // han-allow 업스트림 프롬프트·픽스처
			"id":     intp("直接按单个资产 id 取（可选，与 dsl/type 互斥）"),                                                                                       // han-allow 업스트림 프롬프트·픽스처
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "直接按多个资产 id 取（可选，与 dsl/type 互斥）"}, // han-allow 업스트림 프롬프트·픽스처
			"limit":  intp("返回上限，默认 10（可选）"),                                                                                                       // han-allow 업스트림 프롬프트·픽스처
			"offset": intp("分页偏移，默认 0（可选）"),                                                                                                        // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets 가 켜져 있지 않습니다. AssetStore 가 초기화되지 않았습니다"), nil // han-allow 업스트림 프롬프트·픽스처
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, []int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSLInScope(a.DSL, a.Type, t.taskID, a.Limit, a.Offset)
			default:
				return actool.Errorf("id/ids 를 넘기지 않으면 dsl 은 비울 수 없습니다. 조건 없이 자산 전체를 조회하지 않으니 조회 조건을 주세요"), nil // han-allow 업스트림 프롬프트·픽스처
			}
			if err != nil {
				return actool.Errorf("DSL 오류: " + err.Error()), nil // han-allow 업스트림 프롬프트·픽스처
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies lets an agent enumerate companies (기업) with their scope + asset count.
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"列出资产库中的【企业/公司】及其资产范围(scope)与已归属资产数。用于查看有哪些公司、"+ // han-allow 업스트림 프롬프트·픽스처
			"拿到 company_id（insert_assets 关联 app、list_assets 按 company_id 过滤时用）。"+ // han-allow 업스트림 프롬프트·픽스처
			"可选 search 按公司名模糊过滤(不区分大小写)，留空返回全部。", // han-allow 업스트림 프롬프트·픽스처
		obj(map[string]any{
			"search": str("按公司名模糊过滤(可选，不区分大小写)；留空返回全部"), // han-allow 업스트림 프롬프트·픽스처
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies 가 켜져 있지 않습니다. CompanyStore 가 초기화되지 않았습니다"), nil // han-allow 업스트림 프롬프트·픽스처
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("회사 조회에 실패했습니다: " + err.Error()), nil // han-allow 업스트림 프롬프트·픽스처
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		// list_findings 는 남겨 둡니다. 발견을 보고하기 전에 이 작업의 확인된 발견을 먼저 봐, 같은 발견을 다시 올리지 않게 합니다.
		t.listFindings(),
		t.addFinding(), t.recordFact(),
		// asset management (handlers guard nil store internally)。
		// add_company_scope 는 워커에게 주지 않습니다. 기업 자산 범위를 정하는 일은 플래너/메인/Auto 의 일이고, 워커는 탐색만 실행합니다.
		t.insertAssets(), t.listAssets(),
		// work 사이를 되돌아보기: 워커도 다른 work 의 관찰을 다시 써 같은 일을 반복하지 않습니다.
		// search_all_worker_traces: intent_id 를 미리 몰라도 키워드로 맞은 단계를 전체에서 찾습니다.
		// get_worker_trace: work 하나를 고정한 뒤 단계를 나열하거나, 그 안에서 찾거나, 전체 내용을 가져옵니다.
		t.searchAllWorkerTraces(), t.getWorkerTrace(),
		// node_detail: 워커가 intent_id 나 노드 id 를 받은 뒤 그 노드의 전체 상세를 볼 수 있습니다(위의 되돌아보기와 함께).
		t.nodeDetail(),
		// 아래 도구는 여전히 워커에게 【주지 않고】 플래너/메인에게만 둡니다(맥락을 읽고 work 사이를 복기하는 일은 계획의 책임이고,
		// 워커는 의도 하나를 실행하고 기록만 합니다): list_facts / list_companies / list_worker_traces.
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), // cold-digest §6.1
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work: 사람이 실행 중인 의도(work)에 실시간으로 교정 지시를 넣을 수 있습니다(끊지 않고 진행을 잃지 않음).
		t.steerWorkTool(),
		// set_goals: 사람이 실행 중에 이 작업의 최종 목표를 더할 수 있습니다(플래너가 그것으로 달성 여부를 다시 판단합니다).
		t.setGoals(),
		// set_constraints: 사람이 실행 중에 이 작업의 조작 제약(allow/deny)을 더하거나 고칩니다. 플래너/워커의 탐색 경계를 제한합니다.
		t.setConstraints(),
		// asset management (handlers guard nil store internally)
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets: 필요할 때 이 작업 범위의 미테스트 자산(유형+페이지)을 조회하고, 추가 테스트를 스스로 정합니다.
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}
