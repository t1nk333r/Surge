package cmd

import (
	"bytes"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SurgeDM/Surge/internal/config"
	"github.com/SurgeDM/Surge/internal/testutil"
)

// A running server keeps the settings it started with, and its next settings
// save writes them back over the file. `surge config` therefore asks a running
// local server to reload after saving, so the edit takes effect and survives.
func TestConfigSetReloadsRunningServer(t *testing.T) {
	setupIsolatedCmdState(t)
	resetCommandConnectionState(t)

	var reloads atomic.Int32
	server := testutil.NewHTTPServerT(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/settings/reload" && r.Method == http.MethodPost {
			reloads.Add(1)
			_, _ = w.Write([]byte(`{"status":"reloaded"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	saveActivePort(server.Listener.Addr().(*net.TCPAddr).Port)
	persistAuthToken("test-token")

	var out, errOut bytes.Buffer
	configCmd.SetOut(&out)
	configCmd.SetErr(&errOut)
	t.Cleanup(func() { configCmd.SetOut(nil); configCmd.SetErr(nil) })

	settings, err := config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if err := handleSetOperation(configCmd, settings, []string{"network.proxy_url", "http://127.0.0.1:3128"}); err != nil {
		t.Fatalf("set: %v (stderr %q)", err, errOut.String())
	}
	if reloads.Load() != 1 {
		t.Fatalf("server reloads = %d, want 1 (stderr %q)", reloads.Load(), errOut.String())
	}
	if !strings.Contains(out.String(), "Applied to the running server.") {
		t.Fatalf("stdout = %q", out.String())
	}
	saved, err := config.LoadSettings()
	if err != nil || config.Resolve[string](saved.Network.ProxyURL) != "http://127.0.0.1:3128" {
		t.Fatalf("saved proxy = %v, %v", saved.Network.ProxyURL, err)
	}

	// Setting the value it already has writes nothing and reloads nothing.
	if err := handleSetOperation(configCmd, saved, []string{"network.proxy_url", "http://127.0.0.1:3128"}); err != nil {
		t.Fatalf("same value: %v", err)
	}
	if reloads.Load() != 1 {
		t.Fatalf("an unchanged value reloaded the server")
	}

	// With no server running, saving still succeeds and asks nobody.
	removeActivePort()
	if err := handleSetOperation(configCmd, saved, []string{"network.proxy_url", "default"}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reloads.Load() != 1 {
		t.Fatalf("a reload was attempted with no server running")
	}
}
