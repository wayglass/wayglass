// Package examples embeds the example files that `wayglass init` copies into
// a new project, so the binary works without the source tree.
package examples

import _ "embed"

// Policy is examples/policy.yaml.
//
//go:embed policy.yaml
var Policy []byte
