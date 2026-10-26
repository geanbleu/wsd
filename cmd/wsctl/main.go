package main

import (
	"log/slog"
	"os"
)

func main() {
	slog.New(slog.NewTextHandler(os.Stderr, nil)).Info("wsctl starting")
}
