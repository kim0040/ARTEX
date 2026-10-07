package server

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/mcphttp"
	"github.com/Autumn-27/norma/mcp"
	actool "github.com/Autumn-27/norma/tool"
)

// mcpClient는 연결된 MCP 서버의 공통 표면입니다(stdio, Streamable HTTP,
// 또는 예전 SSE).
// 그래서 tools/list와 정리를 전송 방식과 상관없이 같이 처리합니다.
type mcpClient interface {
	Tools(context.Context) ([]actool.CoreTool, error)
	Close() error
}

// connectMCP는 전송 방식에 맞춰 MCP 서버 하나에 접속합니다. 호출자가 Close해야 합니다.
func connectMCP(ctx context.Context, m *db.MCPServer) (mcpClient, error) {
	switch m.Transport {
	case "stdio":
		if m.Command == "" {
			return nil, fmt.Errorf("stdio 전송에 명령이 없습니다")
		}
		return mcp.NewStdioClient(ctx, m.Name, m.Command, jsonStrMap(m.Env), jsonStrSlice(m.Args)...)
	case "http":
		if m.URL == "" {
			return nil, fmt.Errorf("http 전송에 URL이 없습니다")
		}
		// env 맵은 HTTP 헤더로도 씁니다(예: Authorization).
		return mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	case "sse":
		if m.URL == "" {
			return nil, fmt.Errorf("sse 전송에 URL이 없습니다")
		}
		return mcphttp.NewSSE(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	default:
		return nil, fmt.Errorf("알 수 없는 전송 방식 %q", m.Transport)
	}
}

// discoverAndCacheMCP는 MCP 하나에 접속해 도구를 나열하고, 그 이름을
// mcp_tools_cache에 저장합니다. 화면이 실시간 연결 없이 도구를 보여 주게 합니다.
func (s *Server) discoverAndCacheMCP(ctx context.Context, m *db.MCPServer) error {
	cl, err := connectMCP(ctx, m)
	if err != nil {
		return err
	}
	defer cl.Close()
	ts, err := cl.Tools(ctx)
	if err != nil {
		return err
	}
	tools := make([]db.MCPTool, 0, len(ts))
	for _, t := range ts {
		tools = append(tools, db.MCPTool{Name: t.Name(), Description: t.Description()})
	}
	if err := s.m.pg.SaveMCPTools(m.ID, tools); err != nil {
		return err
	}
	log.Printf("[mcp] %s에서 도구 %d개를 찾아 캐시했습니다", m.Name, len(tools))
	return nil
}

// discoverEmptyMCPsOnStartup은 켜져 있는데 도구 캐시가 아직 없는 MCP를 채웁니다
// (특히 처음 실행 때 심어 둔 브라우저 MCP). 고루틴 하나에서 차례로 돌려,
// stdio 서버(npx)를 한 번에 많이 띄우지 않고, 시작을 막지도 않습니다.
// 실패해도 흐름은 계속합니다. 캐시를 비워 두면 다음 시작 때 다시 시도합니다.
func (s *Server) discoverEmptyMCPsOnStartup() {
	servers, err := s.m.pg.ListMCP()
	if err != nil {
		log.Printf("[mcp] 시작 시 자동 발견: 목록을 읽지 못했습니다: %v", err)
		return
	}
	for _, m := range servers {
		if !m.Enabled || len(m.Tools) > 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
		if err := s.discoverAndCacheMCP(ctx, m); err != nil {
			log.Printf("[mcp] 시작 시 자동 발견 %s 실패: %v", m.Name, err)
		}
		cancel()
	}
}
