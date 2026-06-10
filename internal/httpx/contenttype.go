package httpx

import "strings"

// MediaType extracts the lowercased media type from a Content-Type header,
// dropping any parameters (e.g. "; charset=utf-8").
func MediaType(contentType string) string {
	mt := contentType
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return strings.ToLower(strings.TrimSpace(mt))
}
