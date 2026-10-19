package ai

import "strings"

// OpenAI-compatible providers reject dots in function names. Keep the stable
// dotted registry names at the gateway boundary and encode only on the wire.
func providerToolName(name string) string { return strings.ReplaceAll(name, ".", "__") }
func internalToolName(name string) string { return strings.ReplaceAll(name, "__", ".") }
