package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mcp-http-proxy/config"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type MCPClient struct {
	name     string
	config   config.MCPServer
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	mu       sync.RWMutex
	running  bool
	shutdown chan struct{}
	framing  string // "line" (newline-delimited) or "lsp" (Content-Length framing)
}

type MCPRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type MCPResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *MCPError   `json:"error,omitempty"`
}

type MCPError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func NewMCPClient(name string, serverConfig config.MCPServer) *MCPClient {
	return &MCPClient{
		name:     name,
		config:   serverConfig,
		shutdown: make(chan struct{}),
		framing:  "line",
	}
}

func (c *MCPClient) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		return nil
	}

	// Создаем команду
	c.cmd = exec.CommandContext(ctx, c.config.Command, c.config.Args...)

	// Устанавливаем переменные окружения (с подстановкой ${VAR} из текущего окружения)
	c.cmd.Env = os.Environ()
	for key, value := range c.config.Env {
		expanded := os.ExpandEnv(value)
		c.cmd.Env = append(c.cmd.Env, fmt.Sprintf("%s=%s", key, expanded))
	}

	// Устанавливаем рабочую директорию
	if c.config.WorkDir != "" {
		c.cmd.Dir = c.config.WorkDir
	}

	var err error

	// Создаем pipes для stdio
	c.stdin, err = c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	c.stdout, err = c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	c.stderr, err = c.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Запускаем команду
	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	c.running = true

	// Логируем stderr если включено
	if c.config.Options.LogEnabled {
		go c.logStderr()
	}

	// Отправляем инициализационный запрос
	if err := c.initialize(); err != nil {
		c.Stop()
		return fmt.Errorf("failed to initialize MCP server: %w", err)
	}

	log.Printf("MCP client '%s' started successfully", c.name)
	return nil
}

func (c *MCPClient) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return nil
	}

	close(c.shutdown)
	c.running = false

	if c.stdin != nil {
		c.stdin.Close()
	}

	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}

	log.Printf("MCP client '%s' stopped", c.name)
	return nil
}

func (c *MCPClient) SendRequest(req MCPRequest) (*MCPResponse, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.running {
		return nil, fmt.Errorf("client is not running")
	}

	timeout := time.Duration(c.config.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	framing := c.framing
	if framing == "" {
		framing = "line"
	}

	return c.sendRequestWithFraming(req, framing, timeout)
}

func (c *MCPClient) sendRequestWithFraming(req MCPRequest, framing string, timeout time.Duration) (*MCPResponse, error) {
	// serialize
	reqData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	// debug: outgoing request
	if c.config.Options.LogEnabled {
		log.Printf("[%s] -> %s (framing=%s) payload=%s", c.name, req.Method, framing, string(reqData))
	}

	// writer
	switch framing {
	case "lsp":
		header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(reqData))
		if _, err := c.stdin.Write([]byte(header)); err != nil {
			return nil, fmt.Errorf("failed to write header: %w", err)
		}
		if _, err := c.stdin.Write(reqData); err != nil {
			return nil, fmt.Errorf("failed to send request body: %w", err)
		}
	case "line":
		fallthrough
	default:
		if _, err := c.stdin.Write(append(reqData, '\n')); err != nil {
			return nil, fmt.Errorf("failed to send request: %w", err)
		}
	}

	done := make(chan *MCPResponse, 1)
	errChan := make(chan error, 1)

	go func() {
		defer close(done)
		defer close(errChan)

		reader := bufio.NewReader(c.stdout)

		var raw []byte
		switch framing {
		case "lsp":
			// Read headers
			var contentLength int
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					errChan <- fmt.Errorf("failed to read header line: %w", err)
					return
				}
				line = strings.TrimRight(line, "\r\n")
				if strings.EqualFold(line, "") {
					break
				}
				lower := strings.ToLower(line)
				if strings.HasPrefix(lower, "content-length:") {
					val := strings.TrimSpace(line[len("Content-Length:"):])
					if n, err := strconv.Atoi(val); err == nil {
						contentLength = n
					}
				}
			}
			if contentLength <= 0 {
				errChan <- fmt.Errorf("invalid or missing Content-Length")
				return
			}
			buf := make([]byte, contentLength)
			if _, err := io.ReadFull(reader, buf); err != nil {
				errChan <- fmt.Errorf("failed to read body: %w", err)
				return
			}
			raw = buf
		case "line":
			fallthrough
		default:
			// Read a single JSON line (no token size limit)
			b, err := reader.ReadBytes('\n')
			if err != nil {
				errChan <- fmt.Errorf("failed to read response line: %w", err)
				return
			}
			raw = bytesTrimRightNewline(b)
		}

		// debug: incoming raw
		if c.config.Options.LogEnabled {
			log.Printf("[%s] <- raw (framing=%s) %s", c.name, framing, string(raw))
		}

		var resp MCPResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			errChan <- fmt.Errorf("failed to unmarshal response: %w; raw=%s", err, string(raw))
			return
		}
		done <- &resp
	}()

	select {
	case resp := <-done:
		if resp != nil {
			return resp, nil
		}
		return nil, fmt.Errorf("empty response")
	case err := <-errChan:
		return nil, err
	case <-time.After(timeout):
		return nil, fmt.Errorf("request timeout")
	}
}

func (c *MCPClient) sendNotificationWithFraming(method string, params interface{}) error {
	// build notification message
	msg := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	framing := c.framing
	if framing == "" {
		framing = "line"
	}

	switch framing {
	case "lsp":
		header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
		if _, err := c.stdin.Write([]byte(header)); err != nil {
			return fmt.Errorf("failed to write notification header: %w", err)
		}
		if _, err := c.stdin.Write(data); err != nil {
			return fmt.Errorf("failed to write notification body: %w", err)
		}
	default:
		if _, err := c.stdin.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("failed to write notification: %w", err)
		}
	}

	if c.config.Options.LogEnabled {
		log.Printf("[%s] -> notification %s (framing=%s)", c.name, method, framing)
	}
	return nil
}

func bytesTrimRightNewline(b []byte) []byte {
	// trim trailing \r?\n
	if len(b) == 0 {
		return b
	}
	if b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
		if len(b) > 0 && b[len(b)-1] == '\r' {
			b = b[:len(b)-1]
		}
	}
	return b
}

func (c *MCPClient) initialize() error {
	// Try multiple protocol versions and both framings ("line" first, then "lsp")
	versions := []string{
		"2024-11-05",
		"2025-03-26",
		"2025-06-18",
	}

	var lastErr error
	defaultFraming := c.framing
	if defaultFraming == "" {
		defaultFraming = "line"
	}

	for _, ver := range versions {
		req := MCPRequest{
			JSONRPC: "2.0",
			ID:      1,
			Method:  "initialize",
			Params: map[string]interface{}{
				"protocolVersion": ver,
				"capabilities": map[string]interface{}{
					"tools":     map[string]interface{}{},
					"resources": map[string]interface{}{},
					"prompts":   map[string]interface{}{},
				},
				"clientInfo": map[string]interface{}{
					"name":    "mcp-http-proxy",
					"version": "1.0.0",
				},
			},
		}

		// Try current/default framing first
		if resp, err := c.sendRequestWithFraming(req, defaultFraming, 15*time.Second); err == nil && resp != nil && resp.Error == nil {
			if c.config.Options.LogEnabled {
				log.Printf("[%s] initialized with protocol %s using framing %s", c.name, ver, defaultFraming)
			}
			c.framing = defaultFraming
			// Send notifications/initialized so server starts accepting requests (some servers require this)
			if err := c.sendNotificationWithFraming("notifications/initialized", nil); err != nil && c.config.Options.LogEnabled {
				log.Printf("[%s] failed to send initialized notification: %v", c.name, err)
			}
			return nil
		} else if err != nil && c.config.Options.LogEnabled {
			log.Printf("[%s] initialize failed (framing=%s, ver=%s): %v", c.name, defaultFraming, ver, err)
			if resp != nil && resp.Error != nil {
				log.Printf("[%s] initialize error details: %s", c.name, resp.Error.Message)
			}
		}

		// Fallback to alternate framing
		alt := "lsp"
		if defaultFraming == "lsp" {
			alt = "line"
		}
		if resp, err := c.sendRequestWithFraming(req, alt, 15*time.Second); err == nil && resp != nil && resp.Error == nil {
			if c.config.Options.LogEnabled {
				log.Printf("[%s] initialized with protocol %s using fallback framing %s", c.name, ver, alt)
			}
			c.framing = alt
			// Send notifications/initialized for fallback framing as well
			if err := c.sendNotificationWithFraming("notifications/initialized", nil); err != nil && c.config.Options.LogEnabled {
				log.Printf("[%s] failed to send initialized notification (fallback): %v", c.name, err)
			}
			return nil
		} else if err != nil && c.config.Options.LogEnabled {
			lastErr = err
			log.Printf("[%s] initialize failed (framing=%s, ver=%s): %v", c.name, alt, ver, err)
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("initialization failed")
	}
	return lastErr
}

func (c *MCPClient) logStderr() {
	scanner := bufio.NewScanner(c.stderr)
	for scanner.Scan() {
		log.Printf("[%s] %s", c.name, scanner.Text())
	}
}

func (c *MCPClient) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running
}

func (c *MCPClient) GetName() string {
	return c.name
}
