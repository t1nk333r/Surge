// lint:ignore-leak-check
package scheduler

import (
	"os"
	"testing"

	"github.com/SurgeDM/Surge/internal/testutil"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.CheckLeaks(m.Run(),
		goleak.IgnoreTopFunction("sync.runtime_notifyListWait"),
		goleak.IgnoreTopFunction("github.com/SurgeDM/Surge/internal/scheduler.safeSendProgress"),
	))
}
