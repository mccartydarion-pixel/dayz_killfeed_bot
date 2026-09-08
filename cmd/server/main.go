package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/app"
	"github.com/yourname/dayz-killfeed/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "startup configuration error: %v\n", err)
		os.Exit(1)
	}

	level := slog.LevelInfo
	if strings.EqualFold(strings.TrimSpace(os.Getenv("LOG_LEVEL")), "debug") {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	application, err := app.New(context.Background(), cfg)
	if err != nil {
		slog.Error("component=startup", "msg", "application initialization failed", "err", err.Error())
		os.Exit(1)
	}

	if err := application.Run(); err != nil {
		slog.Error("component=startup", "msg", "application failed", "err", err.Error())
		os.Exit(1)
	}
}
