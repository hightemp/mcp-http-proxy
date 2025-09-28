# mcp-http-proxy

MCP HTTP proxy and SSE server written in golang that aggregates tools from external MCP CLI servers and exposes them over a single HTTP/SSE endpoint.

### Configuration

```yaml
server:
  address: "localhost"
  port: 8080
  name: "MCP HTTP Proxy"
  version: "1.0.0"

mcp_servers:
  filesystem:
    command: "node"
    args: ["/path/to/filesystem-server.js"]
    env:
      NODE_ENV: "production"
    workdir: "/tmp"
    timeout: 30
    options:
      log_enabled: true
      panic_if_invalid: false

  github:
    command: "npx"
    args: ["-y", "@modelcontextprotocol/server-github"]
    env:
      GITHUB_PERSONAL_ACCESS_TOKEN: "your_token_here"
    timeout: 60
    options:
      log_enabled: true
      panic_if_invalid: true
      tool_filter:
        mode: "block"
        list: ["create_or_update_file"]

  database:
    command: "python"
    args: ["-m", "mcp-server-sqlite", "--db-path", "/path/to/database.db"]
    timeout: 45
    options:
      log_enabled: false
      panic_if_invalid: false

  custom_tools:
    command: "/usr/local/bin/custom-mcp-server"
    args: ["--config", "/etc/custom-mcp.conf"]
    env:
      API_KEY: "your_api_key"
      LOG_LEVEL: "info"
    workdir: "/var/lib/custom-mcp"
    timeout: 30
    options:
      log_enabled: true
      panic_if_invalid: false
      tool_filter:
        mode: "allow"
        list: ["search", "analyze", "generate_report"]
```

### License

MIT

![](https://asdertasd.site/counter/mcp-http-proxy)