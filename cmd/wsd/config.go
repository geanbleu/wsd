package main

import "wsd/internal/config"

// loadConfig is a thin wrapper kept for backwards compat with main.go.
var loadConfig = config.Load
