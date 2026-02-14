package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/unify-z/NatAxiom/internal/client"
	"github.com/unify-z/NatAxiom/internal/config"
	"github.com/unify-z/NatAxiom/internal/logx"
)

func main() {
	configPath := flag.String("config", "configs/client.toml", "path to client config")
	flag.Parse()

	cfg, err := config.LoadClientConfig(*configPath)
	if err != nil {
		_, _ = os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(1)
	}

	logger := logx.New(cfg.Client.LogLevel)
	cli := client.New(cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("client stopped", "err", err)
		os.Exit(1)
	}
}
