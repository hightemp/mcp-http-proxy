package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server     ServerConfig         `yaml:"server"`
	MCPServers map[string]MCPServer `yaml:"mcp_servers"`
}

type ServerConfig struct {
	Address string `yaml:"address"`
	Port    int    `yaml:"port"`
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

type MCPServer struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	WorkDir string            `yaml:"workdir"`
	Timeout int               `yaml:"timeout"` // seconds
	Options ServerOptions     `yaml:"options"`
}

type ServerOptions struct {
	LogEnabled     bool    `yaml:"log_enabled"`
	PanicIfInvalid bool    `yaml:"panic_if_invalid"`
	ToolFilter     *Filter `yaml:"tool_filter,omitempty"`
}

type Filter struct {
	Mode string   `yaml:"mode"` // "allow" or "block"
	List []string `yaml:"list"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Устанавливаем значения по умолчанию
	if config.Server.Address == "" {
		config.Server.Address = "localhost"
	}
	if config.Server.Port == 0 {
		config.Server.Port = 8080
	}
	if config.Server.Name == "" {
		config.Server.Name = "MCP HTTP Proxy"
	}
	if config.Server.Version == "" {
		config.Server.Version = "1.0.0"
	}

	return &config, nil
}
