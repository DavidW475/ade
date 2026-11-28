// Package examples embeds the built-in factory templates (doc/factory.md#6-templates).
package examples

import "embed"

// Factories contains factories/*.yaml and the prompts below it.
//
//go:embed factories
var Factories embed.FS
