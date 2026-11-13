// Command codigate-agent connects a private network to the Codigate edge.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/codigate/agent/config"
	"github.com/codigate/agent/tunnel"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "import" {
		os.Exit(runImport(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "access" {
		os.Exit(runAccess(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	path := flag.String("config", envOr("CODIGATE_CONFIG", "codigate.yml"), "path to the agent config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codigate-agent: %v\n", err)
		os.Exit(2)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting codigate-agent", "version", version, "edge", cfg.Edge, "rules", len(cfg.Ingress), "docker", cfg.Docker.Enabled)
	ingress := tunnel.NewIngress(cfg, logger)
	client := &tunnel.Client{
		EdgeURL: cfg.Edge,
		Token:   cfg.Token,
		Version: version,
		Handler: ingress,
		Logger:  logger,
	}
	if cfg.Docker.Enabled {
		d, err := startDiscovery(ctx, cfg, ingress, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codigate-agent: %v\n", err)
			os.Exit(2)
		}
		client.OnSession = d.onSession
	}
	if cfg.EdgeCAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(cfg.EdgeCAFile)
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			fmt.Fprintf(os.Stderr, "codigate-agent: edge_ca_file %q: no usable certificates (%v)\n", cfg.EdgeCAFile, err)
			os.Exit(2)
		}
		client.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	if err := client.Run(ctx); err != nil {
		logger.Error("agent stopped", "err", err)
		os.Exit(1)
	}
	logger.Info("agent stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
