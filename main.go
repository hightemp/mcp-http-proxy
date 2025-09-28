package main

import (
	"flag"
	"log"
	"mcp-http-proxy/config"
	"mcp-http-proxy/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Загружаем конфигурацию
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Создаем и запускаем сервер
	srv, err := server.NewServer(cfg)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	log.Printf("Starting MCP HTTP Proxy on %s", cfg.Server.Address)
	if err := srv.Start(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
