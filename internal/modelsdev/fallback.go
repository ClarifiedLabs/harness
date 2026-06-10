package modelsdev

import (
	"bytes"
	_ "embed"
)

// fallbackAPIJSON is a vendored models.dev api.json snapshot used by setup when
// the network catalog is unavailable.
//
//go:embed fallback_api.json
var fallbackAPIJSON []byte

// Fallback decodes the vendored models.dev api.json snapshot.
func Fallback() (*Catalog, error) {
	return Decode(bytes.NewReader(fallbackAPIJSON))
}
