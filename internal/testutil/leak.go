// Package testutil holds helpers shared by the test suites of several
// packages. It is test-support code that lives in a normal package because Go
// cannot share `_test.go` files across packages.
package testutil

import (
	"fmt"
	"net/http"
	"os"

	"github.com/SurgeDM/Surge/internal/transport"
	"go.uber.org/goleak"
)

// DrainSharedTransports closes the connection pools that outlive an individual
// test. Both `http.DefaultTransport` and Surge's own pool keep idle
// connections, and each idle connection owns a read and a write goroutine that
// only exit when the connection is closed - which a leak check run immediately
// after the last test would otherwise report as leaked.
func DrainSharedTransports() {
	http.DefaultClient.CloseIdleConnections()
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
	transport.DefaultNetworkPool.CloseAll()
}

// CheckLeaks drains the shared transports and, for an otherwise successful
// run, fails it when goroutines are still running. It returns the exit code
// TestMain should use, so a caller that has its own cleanup can run it first.
//
// A failing run is left alone: its leaks are usually consequences of the
// failure, and reporting them would bury the real error.
func CheckLeaks(code int, opts ...goleak.Option) int {
	DrainSharedTransports()
	if code != 0 {
		return code
	}
	if err := goleak.Find(opts...); err != nil {
		fmt.Fprintf(os.Stderr, "goleak: Errors on successful test run: %v\n", err)
		return 1
	}
	return code
}
