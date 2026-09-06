// lint:ignore-leak-check
package orchestrator

import (
	"os"
	"testing"

	"github.com/SurgeDM/Surge/internal/testutil"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.CheckLeaks(m.Run(),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
	))
}
