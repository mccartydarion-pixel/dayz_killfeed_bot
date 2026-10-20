package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	// The runtime image (alpine) carries no time zone database, and the heatmap API loads zones
	// by name (time.LoadLocation): embed the database so that works wherever the binary runs.
	_ "time/tzdata"

	"github.com/yourname/dayz-killfeed/internal/app"
	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/logger"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "startup configuration error: %v\n", err)
		os.Exit(1)
	}

	// JSON by default so Railway parses `level`; LOG_LEVEL / LOG_FORMAT=text
	// are read by internal/logger.
	slog.SetDefault(logger.New())

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
