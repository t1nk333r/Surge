package cmd

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/SurgeDM/Surge/internal/config"
	"github.com/SurgeDM/Surge/internal/service"
	"github.com/SurgeDM/Surge/internal/utils"
	"github.com/google/uuid"
)

// resolveTokenPath returns the path of the auth-token file that this
// process should use.  When the process is running as root/SYSTEM the
// token lives in the system state directory so that the daemon and the
// interactive user look in the same place.
func resolveTokenPath() string {
	if isElevated() {
		return filepath.Join(config.GetSystemStateDir(), "token")
	}
	return filepath.Join(config.GetStateDir(), "token")
}

// resolveRuntimeDir returns the runtime directory this process should use for
// port/PID files.  Mirrors resolveTokenPath: elevated processes (system service
// daemons) write to GetSystemRuntimeDir() so that non-elevated clients running
// getActiveConnectionDetails() can discover them via the system candidate.
func resolveRuntimeDir() string {
	if isElevated() {
		return config.GetSystemRuntimeDir()
	}
	return config.GetRuntimeDir()
}

const serverBindHost = "0.0.0.0"

// findAvailablePortOnHost tries ports starting from start on bindHost.
func findAvailablePortOnHost(bindHost string, start int) (int, net.Listener) {
	for port := start; port < start+100; port++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(bindHost, fmt.Sprintf("%d", port)))
		if err == nil {
			return port, ln
		}
	}
	return 0, nil
}

// findAvailablePort preserves the default all-interface listener used by the
// interactive server and existing callers.
func findAvailablePort(start int) (int, net.Listener) {
	return findAvailablePortOnHost(serverBindHost, start)
}

func bindServerListenerOnHost(portFlag int, bindHost string) (int, net.Listener, error) {
	bindHost = strings.TrimSpace(bindHost)
	if bindHost == "" {
		return 0, nil, fmt.Errorf("bind host cannot be empty")
	}

	if portFlag > 0 {
		address := net.JoinHostPort(bindHost, fmt.Sprintf("%d", portFlag))
		ln, err := net.Listen("tcp", address)
		if err != nil {
			return 0, nil, fmt.Errorf("could not bind to %s: %w", address, err)
		}
		return portFlag, ln, nil
	}
	port, ln := findAvailablePortOnHost(bindHost, 1700)
	if ln == nil {
		return 0, nil, fmt.Errorf("could not find an available port on %s", bindHost)
	}
	return port, ln, nil
}

func bindServerListener(portFlag int) (int, net.Listener, error) {
	return bindServerListenerOnHost(portFlag, serverBindHost)
}

// saveActivePort writes the active port for local CLI and extension discovery.
func saveActivePort(port int) {
	runtimeDir := resolveRuntimeDir()
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		utils.Debug("Error creating runtime directory for port file: %v", err)
		return
	}

	portFile := filepath.Join(runtimeDir, "port")
	if err := os.WriteFile(portFile, []byte(fmt.Sprintf("%d", port)), 0o644); err != nil {
		utils.Debug("Error writing port file: %v", err)
	}
	utils.Debug("HTTP server listening on port %d", port)
}

// removeActivePort cleans up the port file on exit
func removeActivePort() {
	portFile := filepath.Join(resolveRuntimeDir(), "port")
	if err := os.Remove(portFile); err != nil && !os.IsNotExist(err) {
		utils.Debug("Error removing port file: %v", err)
	}
}

var (
	globalHTTPServer   *http.Server
	globalHTTPServerMu sync.Mutex
	activeServerMode   = "local"
)

// startHTTPServer starts the HTTP server using an existing listener
func startHTTPServer(ln net.Listener, port int, defaultOutputDir string, service service.DownloadService, tokenOverride string) {
	authToken := strings.TrimSpace(tokenOverride)
	if authToken == "" {
		authToken = ensureAuthToken()
	} else {
		persistAuthToken(authToken)
	}

	mux := http.NewServeMux()
	registerHTTPRoutes(mux, port, defaultOutputDir, service)

	// Wrap mux with Auth and CORS (CORS outermost to ensure 401/403 include headers)
	handler := corsMiddleware(authMiddleware(authToken, mux))

	server := &http.Server{Handler: handler}
	globalHTTPServerMu.Lock()
	globalHTTPServer = server
	globalHTTPServerMu.Unlock()
	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		utils.Debug("HTTP server error: %v", err)
	}
}

// allowedCORSSchemes are the browser-extension origin schemes that may read
// Surge's API cross-origin.  The Surge extension's background worker is the
// only intended cross-origin client.
var allowedCORSSchemes = [...]string{"chrome-extension", "moz-extension", "safari-web-extension"}

// isAllowedCORSOrigin reports whether origin is permitted to read API
// responses cross-origin: a browser-extension origin, or a loopback origin
// (a locally served UI).  Every other origin — i.e. any web page the user
// happens to visit — gets no CORS headers at all, so the browser refuses to
// expose even the fact that the server answered.
func isAllowedCORSOrigin(origin string) bool {
	u, err := url.Parse(origin)
	// An Origin serialization is scheme://host[:port] and nothing else;
	// anything carrying a path, query, fragment or userinfo is not one.
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}

	for _, scheme := range allowedCORSSchemes {
		if u.Scheme == scheme {
			return true
		}
	}

	if u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && isAllowedCORSOrigin(origin)

		if allowed {
			header := w.Header()
			header.Set("Access-Control-Allow-Origin", origin)
			// The response body depends on the request Origin, so it must not
			// be reused across origins by any cache.
			header.Add("Vary", "Origin")
		}

		// Preflights are unauthenticated by necessity (browsers never attach the
		// bearer token to them) and therefore always terminate here: they must
		// not reach a route handler that could mutate state.
		if r.Method == http.MethodOptions {
			if allowed {
				header := w.Header()
				header.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS, PUT, PATCH")
				header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, Cache-Control, X-Requested-With")
				header.Set("Access-Control-Allow-Private-Network", "true")
				header.Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// authenticatedContextKey marks a request that arrived with a valid bearer
// token.  Only /health needs this: it answers unauthenticated callers too, but
// reveals strictly less to them.
type authenticatedContextKey struct{}

// requestAuthenticated reports whether authMiddleware verified this request's
// bearer token.
func requestAuthenticated(r *http.Request) bool {
	authenticated, _ := r.Context().Value(authenticatedContextKey{}).(bool)
	return authenticated
}

// hasValidBearer compares the request's bearer credential against token in
// constant time.
func hasValidBearer(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	const prefix = "Bearer "
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, prefix) {
		return false
	}
	provided := authHeader[len(prefix):]
	return len(provided) == len(token) && subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated := hasValidBearer(r, token)

		// /health stays open: the CLI and the extension use it to discover a
		// running server before they know the token.  The handler tailors its
		// payload to what the caller has proven.
		if r.URL.Path == "/health" {
			if authenticated {
				r = r.WithContext(context.WithValue(r.Context(), authenticatedContextKey{}, true))
			}
			next.ServeHTTP(w, r)
			return
		}

		// No OPTIONS exemption here on purpose: corsMiddleware wraps this one and
		// already answers every preflight, so an OPTIONS request reaching this
		// point would mean the chain was rewired and must not bypass auth.
		if authenticated {
			next.ServeHTTP(w, r)
			return
		}

		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}

func ensureAuthToken() string {
	tokenFile := resolveTokenPath()
	if token, err := readTokenFromFile(tokenFile); err == nil {
		mirrorTokenToRuntime(token)
		return token
	}

	token := uuid.New().String()
	if err := writeTokenToFile(tokenFile, token); err != nil {
		utils.Debug("Failed to write token file in %s: %v", tokenFile, err)
	}
	mirrorTokenToRuntime(token)
	return token
}

func persistAuthToken(token string) {
	tokenFile := resolveTokenPath()
	if err := writeTokenToFile(tokenFile, token); err != nil {
		utils.Debug("Failed to write token file in %s: %v", tokenFile, err)
	}
	mirrorTokenToRuntime(token)
}

// ensureSystemToken reads (or generates) the token used by the system service
// and returns it.  This is always in GetSystemStateDir regardless of the
// current user — callers that need the system token use this directly.
func ensureSystemToken() (string, error) {
	tokenFile := filepath.Join(config.GetSystemStateDir(), "token")
	if token, err := readTokenFromFile(tokenFile); err == nil {
		mirrorTokenToRuntime(token)
		return token, nil
	}
	token := uuid.New().String()
	if err := writeTokenToFile(tokenFile, token); err != nil {
		return "", fmt.Errorf("failed to write system token to %s: %w", tokenFile, err)
	}
	mirrorTokenToRuntime(token)
	return token, nil
}

// readSystemServiceToken reads the token from the system state directory
// without generating one.  Returns an error if the file doesn't exist or
// isn't readable (the caller may need elevated privileges).
func readSystemServiceToken() (string, error) {
	tokenFile := filepath.Join(config.GetSystemStateDir(), "token")
	return readTokenFromFile(tokenFile)
}

func readTokenFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("empty token file: %s", path)
	}
	return token, nil
}

func writeTokenToFile(path string, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token), 0o600)
}

// mirrorTokenToRuntime writes an owner-only copy of the token to the runtime
// dir so that local CLI clients can auto-discover and connect without needing
// sudo. Same-uid readers are unaffected by 0600; the mode matters because
// GetRuntimeDir falls back to $XDG_STATE_HOME/surge/runtime when
// XDG_RUNTIME_DIR is unset, i.e. outside the 0700 /run/user/<uid> tree.
func mirrorTokenToRuntime(token string) {
	runtimeDir := resolveRuntimeDir()
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return
	}
	// MkdirAll and WriteFile leave an existing dir/file mode alone, and older
	// Surge versions created both world-readable.
	_ = os.Chmod(runtimeDir, 0o700)
	runtimeTokenFile := filepath.Join(runtimeDir, "token")
	if err := os.WriteFile(runtimeTokenFile, []byte(token), 0o600); err != nil {
		return
	}
	_ = os.Chmod(runtimeTokenFile, 0o600)
}
