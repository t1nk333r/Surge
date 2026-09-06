// lint:ignore-leak-check
package probe_test

import (
	"os"
	"testing"

	"github.com/SurgeDM/Surge/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.CheckLeaks(m.Run()))
}
