package client

import "encoding/json"

// jsonUnmarshal is split into a separate file so the client can be
// unit-tested with a mock transport without pulling in real network
// code. Keeping the indirection also makes it easy to swap JSON for
// a different codec later.
func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
