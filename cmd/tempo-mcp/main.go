package main

import (
	"flag"
	"net/http"
	"os"

	"github.com/grafana/dskit/middleware"
	"github.com/grafana/dskit/server"
	"github.com/grafana/tempo/modules/frontend"
	"github.com/grafana/tempo/pkg/util/log"
	zaplogfmt "github.com/jsternberg/zap-logfmt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.yaml.in/yaml/v2"
)

func main() {
	config := zap.NewDevelopmentEncoderConfig()
	logger := zap.New(zapcore.NewCore(
		zaplogfmt.NewEncoder(config),
		os.Stdout,
		zapcore.DebugLevel,
	))
	log.InitLogger(&server.Config{})

	var configPath string
	var listenAddr string
	flag.StringVar(&configPath, "config", "config.yaml", "The path to the Tempo MCP configuration file.")
	flag.StringVar(&listenAddr, "listen", "0.0.0.0:8080", "The listen address of the MCP server.")
	flag.Parse()

	mcpConfig := []frontend.MCPInstanceConfig{}
	if configPath != "" {
		buff, err := os.ReadFile(configPath)
		if err != nil {
			logger.Fatal("failed to read configFile", zap.String("path", configPath), zap.Error(err))
		}

		err = yaml.UnmarshalStrict(buff, &mcpConfig)
		if err != nil {
			logger.Fatal("failed to parse configFile", zap.String("path", configPath), zap.Error(err))
		}
	}

	logger.Info("Starting Tempo MCP server", zap.String("listen", listenAddr))
	mcpServer := frontend.NewMCPServer(&frontend.QueryFrontend{}, "/", log.Logger, middleware.Identity, mcpConfig)

	http.Handle("/", mcpServer)
	err := http.ListenAndServe(listenAddr, nil)
	if err != nil {
		logger.Fatal("error", zap.Error(err))
	}
}
