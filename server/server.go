package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"mcp-http-proxy/client"
	"mcp-http-proxy/config"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

type Server struct {
	config *config.Config

	// mcp-go server + SSE transport
	mcpServer  *mcpserver.MCPServer
	sseServer  *mcpserver.SSEServer
	toolIndex  map[string]ToolRoute // proxyToolName -> route
	indexMutex sync.RWMutex
	builtins   []mcpserver.ServerTool

	// external MCP clients (child servers)
	clients map[string]*client.MCPClient
	mu      sync.RWMutex

	// background sync
	stopSyncCh chan struct{}
	wg         sync.WaitGroup
}

type ToolRoute struct {
	ServerName string
	ToolName   string
}

func NewServer(cfg *config.Config) (*Server, error) {
	s := &Server{
		config:     cfg,
		clients:    make(map[string]*client.MCPClient),
		toolIndex:  make(map[string]ToolRoute),
		stopSyncCh: make(chan struct{}),
	}

	// Create mcp-go MCP server with tool capabilities
	s.mcpServer = mcpserver.NewMCPServer(
		cfg.Server.Name,
		cfg.Server.Version,
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithRecovery(),
	)

	// Built-in echo tool to verify connectivity from Inspector
	echoHandler := func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		msg := request.GetString("message", "")
		return mcp.NewToolResultText(msg), nil
	}
	echoTool := mcp.NewTool("echo",
		mcp.WithDescription("Echo back a message"),
		mcp.WithString("message",
			mcp.Required(),
			mcp.Description("Message to echo"),
		),
	)
	// Register into server and remember as built-in so refresh won't wipe it
	s.mcpServer.AddTool(echoTool, echoHandler)
	s.builtins = append(s.builtins, mcpserver.ServerTool{Tool: echoTool, Handler: echoHandler})

	// Create SSE transport (MCP Inspector uses 'sse' transport)
	publicHost := cfg.Server.Address
	if publicHost == "" || publicHost == "0.0.0.0" || publicHost == "::" {
		publicHost = "localhost"
	}
	baseURL := fmt.Sprintf("http://%s:%d", publicHost, cfg.Server.Port)
	s.sseServer = mcpserver.NewSSEServer(s.mcpServer, mcpserver.WithBaseURL(baseURL))

	// Build child MCP clients from config
	for name, serverCfg := range cfg.MCPServers {
		s.clients[name] = client.NewMCPClient(name, serverCfg)
	}

	return s, nil
}

func (s *Server) Start() error {
	ctx := context.Background()

	// Start all MCP child clients asynchronously to avoid blocking SSE startup
	for name, mcpClient := range s.clients {
		n := name
		c := mcpClient
		go func() {
			if err := c.Start(ctx); err != nil {
				log.Printf("Warning: failed to start MCP client '%s': %v", n, err)
				return
			}
			log.Printf("MCP client '%s' started", n)
		}()
	}

	// Log configured clients
	s.mu.RLock()
	clientNames := make([]string, 0, len(s.clients))
	for n := range s.clients {
		clientNames = append(clientNames, n)
	}
	s.mu.RUnlock()
	log.Printf("Configured MCP clients: %d %v", len(clientNames), clientNames)

	// Initial tools sync in background with retries, so SSE starts immediately
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for i := 0; i < 30; i++ {
			if err := s.refreshToolsOnce(); err != nil {
				log.Printf("Tools sync attempt %d error: %v", i+1, err)
			}
			if len(s.mcpServer.ListTools()) > 0 {
				return
			}
			time.Sleep(1 * time.Second)
		}
		log.Printf("Warning: tools list still empty after initial background sync attempts")
	}()

	// Periodic tools sync
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := s.refreshToolsOnce(); err != nil {
					log.Printf("Tools periodic sync error: %v", err)
				}
			case <-s.stopSyncCh:
				return
			}
		}
	}()

	// Start SSE server
	addr := fmt.Sprintf("%s:%d", s.config.Server.Address, s.config.Server.Port)
	log.Printf("Starting MCP SSE server on http://%s (endpoints: /sse, /message)", addr)
	return s.sseServer.Start(addr)
}

func (s *Server) Stop() error {
	// stop periodic sync
	close(s.stopSyncCh)
	s.wg.Wait()

	// shutdown SSE
	if s.sseServer != nil {
		_ = s.sseServer.Shutdown(context.Background())
	}

	// stop child clients
	s.mu.RLock()
	for _, c := range s.clients {
		_ = c.Stop()
	}
	s.mu.RUnlock()

	return nil
}

// refreshToolsOnce pulls tools from all running child servers and registers them in mcpServer
func (s *Server) refreshToolsOnce() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var tools []mcpserver.ServerTool
	newIndex := make(map[string]ToolRoute)

	for serverName, cl := range s.clients {
		if !cl.IsRunning() {
			continue
		}

		req := client.MCPRequest{
			JSONRPC: "2.0",
			ID:      1,
			Method:  "tools/list",
		}

		resp, err := cl.SendRequest(req)
		if err != nil {
			log.Printf("Failed to get tools from '%s': %v", serverName, err)
			continue
		}
		if resp.Error != nil {
			log.Printf("Error from '%s': %s", serverName, resp.Error.Message)
			continue
		}

		resultMap, ok := resp.Result.(map[string]interface{})
		if !ok {
			continue
		}
		rawList, ok := resultMap["tools"].([]interface{})
		if !ok {
			continue
		}
		log.Printf("Server '%s' tools: %d", serverName, len(rawList))

		for _, it := range rawList {
			tm, ok := it.(map[string]interface{})
			if !ok {
				continue
			}
			origName, _ := safeString(tm["name"])
			if origName == "" {
				continue
			}
			desc, _ := safeString(tm["description"])

			proxyName := fmt.Sprintf("%s_%s", serverName, origName)

			tool := mcp.Tool{
				Name:        proxyName,
				Description: desc,
				InputSchema: buildInputSchema(tm["inputSchema"]),
			}

			// Register proxy handler
			tools = append(tools, mcpserver.ServerTool{
				Tool:    tool,
				Handler: s.makeProxyHandler(serverName, origName),
			})

			newIndex[proxyName] = ToolRoute{
				ServerName: serverName,
				ToolName:   origName,
			}
		}
	}

	// Always include built-in tools so they don't disappear on empty proxy lists
	if len(s.builtins) > 0 {
		tools = append(tools, s.builtins...)
	}

	if len(tools) > 0 {
		// Replace tools atomically in mcpServer (proxies + built-ins)
		s.mcpServer.SetTools(tools...)

		// Replace index (only for proxy tools - built-ins not indexed here)
		s.indexMutex.Lock()
		s.toolIndex = newIndex
		s.indexMutex.Unlock()

		log.Printf("Aggregated tools to register: %d (including %d built-ins)", len(tools), len(s.builtins))
	} else {
		// Keep whatever is currently registered (e.g., built-ins) to avoid wiping list
		log.Printf("No proxy tools discovered; keeping existing tools (built-ins: %d)", len(s.builtins))
	}

	return nil
}

func (s *Server) makeProxyHandler(serverName, origTool string) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Resolve route by current tool name (req.Params.Name)
		s.indexMutex.RLock()
		rt, ok := s.toolIndex[req.Params.Name]
		s.indexMutex.RUnlock()
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("route not found for tool '%s'", req.Params.Name)), nil
		}

		// Fetch client
		s.mu.RLock()
		cl, exists := s.clients[rt.ServerName]
		s.mu.RUnlock()
		if !exists || !cl.IsRunning() {
			return mcp.NewToolResultError(fmt.Sprintf("server '%s' not available", rt.ServerName)), nil
		}

		// Prepare proxied call
		args := req.Params.Arguments
		clientReq := client.MCPRequest{
			JSONRPC: "2.0",
			ID:      1,
			Method:  "tools/call",
			Params: map[string]interface{}{
				"name":      rt.ToolName,
				"arguments": args,
			},
		}

		resp, err := cl.SendRequest(clientReq)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("proxy call error: %v", err)), nil
		}
		if resp.Error != nil {
			// pass-through error
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Type: "text",
						Text: fmt.Sprintf("Upstream error (%d): %s", resp.Error.Code, resp.Error.Message),
					},
				},
				IsError: true,
			}, nil
		}

		// Try to decode upstream result into mcp.CallToolResult
		if resp.Result == nil {
			return mcp.NewToolResultError("empty result from upstream"), nil
		}
		raw, err := json.Marshal(resp.Result)
		if err == nil {
			var out mcp.CallToolResult
			if err := json.Unmarshal(raw, &out); err == nil {
				// Successfully converted structured result
				return &out, nil
			}
		}

		// Fallback: text content with raw dump
		return mcp.NewToolResultText(fmt.Sprintf("%v", resp.Result)), nil
	}
}

func buildInputSchema(v interface{}) mcp.ToolInputSchema {
	schema := mcp.ToolInputSchema{
		Type:       "object",
		Properties: map[string]any{},
		Required:   []string{},
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return schema
	}

	if t, ok := m["type"].(string); ok && t != "" {
		schema.Type = t
	}
	if props, ok := m["properties"].(map[string]interface{}); ok {
		schema.Properties = props
	}
	if req, ok := m["required"].([]interface{}); ok {
		var r []string
		for _, x := range req {
			if s, ok := x.(string); ok {
				r = append(r, s)
			}
		}
		schema.Required = r
	}

	// allow additionalProperties passthrough if present
	if ap, ok := m["additionalProperties"]; ok {
		if schema.Properties == nil {
			schema.Properties = map[string]any{}
		}
		schema.Properties["additionalProperties"] = ap
	}

	return schema
}

func safeString(v interface{}) (string, bool) {
	if v == nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return fmt.Sprintf("%v", v), true
}
