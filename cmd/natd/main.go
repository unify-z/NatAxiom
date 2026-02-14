package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/unify-z/NatAxiom/internal/config"
	"github.com/unify-z/NatAxiom/internal/logx"
	"github.com/unify-z/NatAxiom/internal/server"
)

func main() {
	configPath := flag.String("config", "configs/server.toml", "path to server config")
	flag.Parse()

	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		_, _ = os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(1)
	}

	logger := logx.New(cfg.Server.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(cfg, logger)
	if err := srv.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
