// lint:ignore-leak-check
package hls

import (
	"testing"

	"go.uber.org/goleak"
)

// The parser starts no goroutines; goleak is here because the project enforces
// it for every package that has tests, so a future addition cannot leak one
// unnoticed.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
