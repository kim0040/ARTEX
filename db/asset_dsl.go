package db

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Expr는 DSL 잎 절 하나다. 자산 그래프를 글 조건으로 거를 때 쓴다.
type Expr struct {
	Field string // 비어 있으면 맨 글 전문 검색
	Op    string // "=", "==", "!=", ">", ">=", "<", "<="
	Value string
}

// astNode는 파싱된 DSL 식 트리의 노드다.
type astNode struct {
	kind     string // "and", "or", "leaf" (그리고, 또는, 잎)
	children []*astNode
	expr     *Expr // "leaf"일 때만
}

func andNode(cs []*astNode) *astNode { return &astNode{kind: "and", children: cs} }
func orNode(cs []*astNode) *astNode  { return &astNode{kind: "or", children: cs} }
func leafNode(e Expr) *astNode       { return &astNode{kind: "leaf", expr: &e} }

// knownStringFields는 DSL 필드 이름 → SQL 열 이름이다.
// 참고: "type"은 일부러 빼 둔다. DSL 필드가 아니라 따로 받는 매개변수다.
var knownStringFields = map[string]string{
	"domain":       "domain",
	"root_domain":  "root_domain",
	"ip":           "ip",
	"url":          "url",
	"page_title":   "page_title",
	"title":        "page_title",
	"icp":          "icp",
	"service_name": "service_name",
	"app_name":     "app_name",
	"bundle_id":    "bundle_id",
	"category":     "category",
	"app_icp":      "app_icp",
	"method":       "method",
	"service_type": "service_type",
	"record_type":  "record_type",
}

// knownArrayFields는 DSL 필드 이름 → SQL 열 이름(배열)이다.
var knownArrayFields = map[string]string{
	"technology":   "technologies",
	"technologies": "technologies",
	"tech":         "technologies",
}

// knownNumericFields는 DSL 필드 이름 → SQL 열 이름(정수)이다.
var knownNumericFields = map[string]string{
	"port":        "port",
	"status_code": "status_code",
	"status":      "status_code",
}

func isKnownField(f string) bool {
	f = strings.ToLower(f)
	_, s := knownStringFields[f]
	_, a := knownArrayFields[f]
	_, n := knownNumericFields[f]
	return s || a || n || f == "company_id" || f == "task_id"
}

// ── 토크나이저 ───────────────────────────────────────────────────────────────

const (
	tkField = "FIELD"
	tkBare  = "BARE"
	tkAnd   = "AND"
	tkOr    = "OR"
	tkLP    = "LPAREN"
	tkRP    = "RPAREN"
	tkEOF   = "EOF"
)

type tok struct {
	kind string
	expr *Expr // tkField와 tkBare일 때 채운다
}

func tokenize(s string) ([]tok, error) {
	var tokens []tok
	i := 0
	for i < len(s) {
		for i < len(s) && unicode.IsSpace(rune(s[i])) {
			i++
		}
		if i >= len(s) {
			break
		}
		switch s[i] {
		case '(':
			tokens = append(tokens, tok{kind: tkLP})
			i++
		case ')':
			tokens = append(tokens, tok{kind: tkRP})
			i++
		default:
			if expr, end, ok := tryParseFieldExpr(s, i); ok {
				tokens = append(tokens, tok{kind: tkField, expr: &expr})
				i = end
				continue
			}
			word, end := readToken(s, i)
			if word == "" {
				i++
				continue
			}
			switch strings.ToUpper(word) {
			case "AND":
				tokens = append(tokens, tok{kind: tkAnd})
			case "OR":
				tokens = append(tokens, tok{kind: tkOr})
			default:
				e := Expr{Field: "", Op: "=", Value: word}
				tokens = append(tokens, tok{kind: tkBare, expr: &e})
			}
			i = end
		}
	}
	tokens = append(tokens, tok{kind: tkEOF})
	return tokens, nil
}

// ── 파서 ───────────────────────────────────────────────────────────────────
//
// 문법 (AND가 OR보다 더 세게 묶인다):
// expr     = or_expr                 식
// or_expr  = and_expr (OR and_expr)*  또는
// and_expr = atom    (AND atom)*      그리고
// atom     = FIELD | BARE | '(' expr ')'  원자

type dslParser struct {
	tokens []tok
	pos    int
}

func (p *dslParser) peek() tok {
	if p.pos >= len(p.tokens) {
		return tok{kind: tkEOF}
	}
	return p.tokens[p.pos]
}

func (p *dslParser) consume() tok {
	t := p.peek()
	p.pos++
	return t
}

func (p *dslParser) parseOr() (*astNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	children := []*astNode{left}
	for p.peek().kind == tkOr {
		p.consume()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		children = append(children, right)
	}
	if len(children) == 1 {
		return children[0], nil
	}
	return orNode(children), nil
}

func (p *dslParser) parseAnd() (*astNode, error) {
	left, err := p.parseAtom()
	if err != nil {
		return nil, err
	}
	children := []*astNode{left}
	for p.peek().kind == tkAnd {
		p.consume()
		right, err := p.parseAtom()
		if err != nil {
			return nil, err
		}
		children = append(children, right)
	}
	if len(children) == 1 {
		return children[0], nil
	}
	return andNode(children), nil
}

func (p *dslParser) parseAtom() (*astNode, error) {
	t := p.peek()
	switch t.kind {
	case tkField, tkBare:
		p.consume()
		return leafNode(*t.expr), nil
	case tkLP:
		p.consume()
		node, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tkRP {
			return nil, fmt.Errorf("DSL 문법 오류: 오른쪽 괄호 ')' 가 없다")
		}
		p.consume()
		return node, nil
	case tkEOF:
		return nil, fmt.Errorf("DSL 문법 오류: 표현식이 완전하지 않다")
	default:
		return nil, fmt.Errorf("DSL 문법 오류: 예상하지 못한 token '%s'", t.kind)
	}
}

// ParseDSL은 DSL 조회 문자열을 식 트리로 파싱한다.
//
// 문법:
//
//	field=value      비슷한 맞춤 (ILIKE '%value%')
//	field==value     정확한 맞춤
//	field!=value     비슷한 맞춤 제외
//	port>8080        숫자 비교
//	bare word        주요 글 필드 전체의 전문 비슷한 맞춤
//
// 연산자: AND OR (대소문자 무시), 묶음은 괄호.
// AND가 OR보다 더 세게 묶인다.
func ParseDSL(s string) (*astNode, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	tokens, err := tokenize(s)
	if err != nil {
		return nil, err
	}
	p := &dslParser{tokens: tokens}
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tkEOF {
		return nil, fmt.Errorf("DSL 문법 오류: 예상하지 못한 내용 '%s'", p.peek().kind)
	}
	return node, nil
}

// ── SQL 만들기 ──────────────────────────────────────────────────────────────

// fullTextCols는 맨 글 토큰을 찾을 열이다.
var fullTextCols = []string{
	"domain", "root_domain", "ip", "url", "page_title",
	"icp", "service_name", "app_name", "app_description",
}

type whereBuilder struct {
	args []any
	base int // 자리표시자는 base+1, base+2, … 번호. 0이면 보통의 $1, $2, …
}

func (b *whereBuilder) next(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", b.base+len(b.args))
}

func (b *whereBuilder) build(node *astNode) (string, error) {
	switch node.kind {
	case "and":
		parts := make([]string, 0, len(node.children))
		for _, child := range node.children {
			clause, err := b.build(child)
			if err != nil {
				return "", err
			}
			parts = append(parts, "("+clause+")")
		}
		return strings.Join(parts, " AND "), nil
	case "or":
		parts := make([]string, 0, len(node.children))
		for _, child := range node.children {
			clause, err := b.build(child)
			if err != nil {
				return "", err
			}
			parts = append(parts, "("+clause+")")
		}
		return strings.Join(parts, " OR "), nil
	case "leaf":
		return b.buildLeaf(*node.expr)
	}
	return "", fmt.Errorf("unknown node kind: %s", node.kind)
}

func (b *whereBuilder) buildLeaf(e Expr) (string, error) {
	f := strings.ToLower(e.Field)

	// 맨 글: 모든 글 필드와 배열을 OR로 잇는다
	if f == "" {
		p := b.next("%" + e.Value + "%")
		var parts []string
		for _, col := range fullTextCols {
			parts = append(parts, col+" ILIKE "+p)
		}
		parts = append(parts,
			"EXISTS (SELECT 1 FROM unnest(technologies) t(v) WHERE v ILIKE "+p+")",
			"EXISTS (SELECT 1 FROM unnest(bound_domains) t(v) WHERE v ILIKE "+p+")",
		)
		return "(" + strings.Join(parts, " OR ") + ")", nil
	}

	// task_id: $N = ANY(task_ids) 작업 id가 배열에 있는지
	if f == "task_id" {
		n, err := strconv.ParseInt(e.Value, 10, 64)
		if err != nil {
			return "", fmt.Errorf("task_id 는 정수 값이어야 한다: %s", e.Value)
		}
		return b.next(n) + " = ANY(task_ids)", nil
	}

	// company_id: 정수 정확히 맞춤
	if f == "company_id" {
		n, err := strconv.ParseInt(e.Value, 10, 64)
		if err != nil {
			return "", fmt.Errorf("company_id 는 정수 값이어야 한다: %s", e.Value)
		}
		return "company_id = " + b.next(n), nil
	}

	// 숫자 필드
	if col, ok := knownNumericFields[f]; ok {
		n, err := strconv.Atoi(e.Value)
		if err != nil {
			return "", fmt.Errorf("필드 %s 는 정수 값이어야 한다: %s", f, e.Value)
		}
		op := e.Op
		if op == "==" {
			op = "="
		}
		if op != "=" && op != "!=" && op != ">" && op != ">=" && op != "<" && op != "<=" {
			return "", fmt.Errorf("필드 %s 는 연산자 %s 를 지원하지 않는다", f, e.Op)
		}
		return fmt.Sprintf("%s %s %s", col, op, b.next(n)), nil
	}

	// 배열 필드
	if col, ok := knownArrayFields[f]; ok {
		switch e.Op {
		case "==":
			return b.next(e.Value) + " = ANY(" + col + ")", nil
		case "!=":
			return "NOT (" + b.next(e.Value) + " = ANY(" + col + "))", nil
		case "=":
			p := b.next("%" + e.Value + "%")
			return "EXISTS (SELECT 1 FROM unnest(" + col + ") t(v) WHERE v ILIKE " + p + ")", nil
		default:
			return "", fmt.Errorf("배열 필드 %s 는 연산자 %s 를 지원하지 않는다", f, e.Op)
		}
	}

	// 문자열 필드
	if col, ok := knownStringFields[f]; ok {
		switch e.Op {
		case "=":
			return col + " ILIKE " + b.next("%"+e.Value+"%"), nil
		case "==":
			return col + " = " + b.next(e.Value), nil
		case "!=":
			return col + " NOT ILIKE " + b.next("%"+e.Value+"%"), nil
		default:
			return "", fmt.Errorf("문자열 필드 %s 는 연산자 %s 를 지원하지 않는다", f, e.Op)
		}
	}

	return "", fmt.Errorf("알 수 없는 필드: %s", f)
}

func buildDSLWhere(node *astNode) (string, []any, error) {
	return buildDSLWhereBase(node, 0)
}

// buildDSLWhereBase는 자리표시자 시작 번호를 받는 buildDSLWhere다. 나오는 인자는
// base+1부터 번호를 매겨, $1..$base는 호출자에게 남긴다(예: 범위 CTE가
// $1을 작업 id용으로 잡아 둔다).
func buildDSLWhereBase(node *astNode, base int) (string, []any, error) {
	if node == nil {
		return "1=1", nil, nil
	}
	b := &whereBuilder{base: base}
	clause, err := b.build(node)
	if err != nil {
		return "", nil, err
	}
	return clause, b.args, nil
}

// ── 도우미 (파서와 같이 씀) ─────────────────────────────────────────────

// tryParseFieldExpr는 pos에서 "필드 연산 값"을 파싱해 본다.
func tryParseFieldExpr(s string, pos int) (Expr, int, bool) {
	i := pos
	if i >= len(s) || !isIdentStart(s[i]) {
		return Expr{}, pos, false
	}
	for i < len(s) && isIdentChar(s[i]) {
		i++
	}
	field := strings.ToLower(s[pos:i])
	if !isKnownField(field) {
		return Expr{}, pos, false
	}
	if i >= len(s) {
		return Expr{}, pos, false
	}
	var op string
	switch {
	case i+1 < len(s) && (s[i] == '=' || s[i] == '!' || s[i] == '>' || s[i] == '<') && s[i+1] == '=':
		op = s[i : i+2]
		i += 2
	case s[i] == '>' || s[i] == '<' || s[i] == '=':
		op = string(s[i])
		i++
	default:
		return Expr{}, pos, false
	}
	value, end := readToken(s, i)
	if end == i {
		return Expr{}, pos, false
	}
	return Expr{Field: field, Op: op, Value: value}, end, true
}

// readToken은 pos에서 시작하는 따옴표 있는/없는 토큰을 읽는다.
func readToken(s string, pos int) (string, int) {
	if pos >= len(s) {
		return "", pos
	}
	if s[pos] == '"' {
		i := pos + 1
		for i < len(s) && s[i] != '"' {
			i++
		}
		val := s[pos+1 : i]
		if i < len(s) {
			i++
		}
		return val, i
	}
	i := pos
	for i < len(s) && !unicode.IsSpace(rune(s[i])) && s[i] != '(' && s[i] != ')' {
		i++
	}
	return s[pos:i], i
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// ── QueryDSL 조회 ────────────────────────────────────────────────────────────

// ValidateDSL은 데이터베이스를 건드리지 않고 DSL 식을 파싱하고 컴파일한다.
// HTTP 호출자는 이것으로 클라이언트 문법 오류와 조회
// 실패를 가른다. 조회 실패는 서버 오류로 남아야 한다.
func ValidateDSL(dsl string) error {
	node, err := ParseDSL(dsl)
	if err != nil {
		return err
	}
	_, _, err = buildDSLWhere(node)
	return err
}

// CountDSL은 DSL 식(그리고 선택인 유형)에 맞는 자산 총수를 돌려준다.
// 서버 쪽 페이지용이다. QueryDSL과 같은 WHERE이고 LIMIT/OFFSET은 없다.
// taskID가 0보다 크면 그 작업에 붙은 자산만 센다.
func (s *AssetStore) CountDSL(dsl, typ string, taskID int64) (int, error) {
	node, err := ParseDSL(dsl)
	if err != nil {
		return 0, err
	}
	where, args, err := buildDSLWhere(node)
	if err != nil {
		return 0, err
	}
	if typ != "" {
		args = append(args, typ)
		where += fmt.Sprintf(" AND type = $%d", len(args))
	}
	if taskID > 0 {
		args = append(args, taskID)
		where += fmt.Sprintf(" AND $%d = ANY(task_ids)", len(args))
	}
	var n int
	err = s.db.QueryRow("SELECT count(*) FROM assets WHERE "+where, args...).Scan(&n)
	return n, err
}

// QueryDSL은 DSL 조회 문자열을 자산 저장소에 실행한다.
// typ은 DSL 식과 따로 적용하는 선택 자산 유형 필터다.
// taskID가 0보다 크면 그 작업에 붙은 자산만 보고, 각
// 행의 작업별 출처 메타데이터를 채운다(QueryByTask와 같다).
func (s *AssetStore) QueryDSL(dsl, typ string, taskID int64, limit, offset int) ([]*Asset, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	node, err := ParseDSL(dsl)
	if err != nil {
		return nil, err
	}
	where, args, err := buildDSLWhere(node)
	if err != nil {
		return nil, err
	}
	if typ != "" {
		args = append(args, typ)
		where += fmt.Sprintf(" AND type = $%d", len(args))
	}
	if taskID > 0 {
		args = append(args, taskID)
		where += fmt.Sprintf(" AND $%d = ANY(task_ids)", len(args))
	}
	args = append(args, limit, offset)
	q := assetSelectCols + " WHERE " + where +
		fmt.Sprintf(" ORDER BY last_seen DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets, err := scanAssets(rows)
	if err != nil {
		return nil, err
	}
	if taskID > 0 {
		if err := s.hydrateTaskAssetSources(taskID, assets); err != nil {
			return nil, err
		}
	}
	return assets, nil
}

// QueryDSLInScope 는 QueryDSL 을 taskID 의(그리고 그 직접 원본 작업들의) 선언된 범위에 속하는 자산으로 제한한다. 소속이지 리터럴 값이 아니다. 즉
// root_domain 범위는 그 아래의 모든 subdomain / service / endpoint 를 돌려준다. 이것은
// 에이전트가 쓰는 list_assets 경로라서, 에이전트는 공유 자산 그래프 전체가 아니라 그 작업과 관련된 자산을 조회한다.
// taskID<=0 (작업이 아닌 맥락: Auto / pentest / chat)
// 에는 지킬 범위가 없어서 평범한 전역 QueryDSL 로 돌아간다. 각 행은
// QueryByTask 와 같은 작업별 출처 메타데이터를 담는다.
func (s *AssetStore) QueryDSLInScope(dsl, typ string, taskID int64, limit, offset int) ([]*Asset, error) {
	if taskID <= 0 {
		return s.QueryDSL(dsl, typ, 0, limit, offset)
	}
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	node, err := ParseDSL(dsl)
	if err != nil {
		return nil, err
	}
	// $1은 taskID용으로 남겨 둔다(scopeTargetCTE). DSL 자리표시자는 $2부터다.
	where, dslArgs, err := buildDSLWhereBase(node, 1)
	if err != nil {
		return nil, err
	}
	args := []any{taskID}
	args = append(args, dslArgs...)
	if typ != "" {
		args = append(args, typ)
		where += fmt.Sprintf(" AND type = $%d", len(args))
	}
	where += " AND id IN (SELECT id FROM target)"
	args = append(args, limit, offset)
	q := `WITH ` + scopeTargetCTE + ` ` + assetSelectCols + " WHERE " + where +
		fmt.Sprintf(" ORDER BY last_seen DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets, err := scanAssets(rows)
	if err != nil {
		return nil, err
	}
	if err := s.hydrateTaskAssetSources(taskID, assets); err != nil {
		return nil, err
	}
	return assets, nil
}

// GetByIDsInScope는 GetByIDs를, taskID(와 직접 원본 작업)가 선언한 범위에
// 속하는 id만 보게 제한한다. 그래서 에이전트가 id로 범위 밖 자산에 닿지 못한다.
// taskID가 0 이하면(작업이 아닌 맥락) 전역 GetByIDs로 내려간다.
// 범위 밖 id는 오류가 아니라 결과에서 조용히 빠진다.
func (s *AssetStore) GetByIDsInScope(taskID int64, ids []int64) ([]*Asset, error) {
	if taskID <= 0 {
		return s.GetByIDs(ids)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, taskID) // $1은 scopeTargetCTE용으로 남겨 둔다
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	q := `WITH ` + scopeTargetCTE + ` ` + assetSelectCols +
		" WHERE id IN (" + strings.Join(placeholders, ",") + ")" +
		" AND id IN (SELECT id FROM target) ORDER BY last_seen DESC, id DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets, err := scanAssets(rows)
	if err != nil {
		return nil, err
	}
	if err := s.hydrateTaskAssetSources(taskID, assets); err != nil {
		return nil, err
	}
	return assets, nil
}
