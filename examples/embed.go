// Package examples bettet die mitgelieferten Fabrik-Vorlagen ein (doc/factory.md#6-vorlagen).
package examples

import "embed"

// Factories enthält factories/*.yaml und die Prompts darunter.
//
//go:embed factories
var Factories embed.FS
