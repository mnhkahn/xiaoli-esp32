package wakeword

import "math"

// init wires the sqrtImpl indirection to math.Sqrt. Kept in a
// separate file so test files can override it before TestMain runs.
func init() {
	sqrtImpl = math.Sqrt
}
