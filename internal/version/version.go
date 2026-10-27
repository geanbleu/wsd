// Package version porte la version de build, injectée par le Makefile.
package version

import "runtime"

// Version est remplacée au build via -ldflags "-X wsd/internal/version.Version=...".
var Version = "dev"

// String renvoie une ligne de version lisible, pour le flag -version des binaires.
func String(prog string) string {
	return prog + " " + Version + " (" + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + ")"
}
