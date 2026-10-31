// Package configs holds compiled-in deployment assets.
package configs

import _ "embed"

// NSJailPythonTemplate is the nsjail configuration template for python runs.
// It is embedded so the server always has a valid template even when deployed
// without the configs/ directory.
//
//go:embed nsjail_python.cfg.tmpl
var NSJailPythonTemplate []byte
