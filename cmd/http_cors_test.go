package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testExtensionOrigin is the origin a Chrome build of the Surge extension
// sends (Firefox sends moz-extension://<uuid>).
const testExtensionOrigin = "chrome-extension://abcdefghijklmnopabcdefghijklmnop"

const corsTestToken = "cors-test-token"

// newCORSTestHandler wires the production middleware chain around the real
// routes so the tests observe what a browser would see.
func newCORSTestHandler(t *testing.T, port int) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	registerHTTPRoutes(mux, port, "", &httpAPITestService{})
	return corsMiddleware(authMiddleware(corsTestToken, mux))
}

func TestCORSOriginPolicy(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		allowed bool
	}{
		{name: "chrome extension", origin: testExtensionOrigin, allowed: true},
		{name: "firefox extension", origin: "moz-extension://3c4d5e6f-7a8b-9c0d-1e2f-3a4b5c6d7e8f", allowed: true},
		{name: "safari extension", origin: "safari-web-extension://A1B2C3D4-E5F6", allowed: true},
		{name: "loopback ip with port", origin: "http://127.0.0.1:1700", allowed: true},
		{name: "localhost without port", origin: "http://localhost", allowed: true},
		{name: "ipv6 loopback", origin: "http://[::1]:1700", allowed: true},
		{name: "public https origin", origin: "https://evil.example", allowed: false},
		{name: "public http origin", origin: "http://evil.example", allowed: false},
		{name: "origin whose host merely ends in localhost", origin: "http://notlocalhost", allowed: false},
		{name: "attacker subdomain prefixed with loopback name", origin: "http://localhost.evil.example", allowed: false},
		{name: "https loopback is not reachable on this server", origin: "https://127.0.0.1:1700", allowed: false},
		{name: "opaque origin", origin: "null", allowed: false},
		{name: "file origin", origin: "file://", allowed: false},
		{name: "extension scheme with path payload", origin: testExtensionOrigin + "/evil", allowed: false},
		{name: "empty origin", origin: "", allowed: false},
	}

	handler := newCORSTestHandler(t, 1700)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			// The request itself always succeeds; only cross-origin
			// readability changes.
			assert.Equal(t, http.StatusOK, rec.Code)

			if tt.allowed {
				assert.Equal(t, tt.origin, rec.Header().Get("Access-Control-Allow-Origin"))
				assert.Equal(t, "Origin", rec.Header().Get("Vary"))
			} else {
				assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
				assert.Empty(t, rec.Header().Get("Vary"))
				assert.Empty(t, rec.Header().Get("Access-Control-Allow-Private-Network"))
			}
		})
	}
}

func TestCORSPreflight(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		allowed bool
	}{
		{name: "extension origin", origin: testExtensionOrigin, allowed: true},
		{name: "loopback origin", origin: "http://127.0.0.1:1700", allowed: true},
		{name: "public origin", origin: "https://evil.example", allowed: false},
	}

	handler := newCORSTestHandler(t, 1700)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// /download mutates state: the preflight must never reach it, and
			// carries no bearer token because browsers never attach one.
			req := httptest.NewRequest(http.MethodOptions, "/download", nil)
			req.Header.Set("Origin", tt.origin)
			req.Header.Set("Access-Control-Request-Method", "POST")
			req.Header.Set("Access-Control-Request-Private-Network", "true")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Contains(t, []int{http.StatusOK, http.StatusNoContent}, rec.Code,
				"preflight must be answered without auth")
			assert.Empty(t, rec.Body.String(), "preflight must have no body")

			if tt.allowed {
				assert.Equal(t, tt.origin, rec.Header().Get("Access-Control-Allow-Origin"))
				assert.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Private-Network"))
				assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "POST")
				assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization")
			} else {
				assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
				assert.Empty(t, rec.Header().Get("Access-Control-Allow-Private-Network"))
				assert.Empty(t, rec.Header().Get("Access-Control-Allow-Methods"))
			}
		})
	}
}

func TestCORSPrivateNetworkHeaderOnlyOnPreflight(t *testing.T) {
	handler := newCORSTestHandler(t, 1700)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", testExtensionOrigin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, testExtensionOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Private-Network"),
		"private-network grant belongs on the preflight response only")
}

func TestHealthPortRequiresAuth(t *testing.T) {
	const port = 1723
	handler := newCORSTestHandler(t, port)

	tests := []struct {
		name        string
		authHeader  string
		expectPort  bool
		description string
	}{
		{name: "no token", authHeader: "", expectPort: false},
		{name: "wrong token", authHeader: "Bearer not-the-token", expectPort: false},
		{name: "malformed scheme", authHeader: corsTestToken, expectPort: false},
		{name: "valid token", authHeader: "Bearer " + corsTestToken, expectPort: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, "/health must stay reachable for discovery")

			var payload map[string]interface{}
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&payload))

			// Liveness and mode stay public: the extension's testConnection
			// flow reads mode before it has a verified token.
			assert.Equal(t, "ok", payload["status"])
			assert.Equal(t, activeServerMode, payload["mode"])

			if tt.expectPort {
				assert.Equal(t, float64(port), payload["port"])
			} else {
				assert.NotContains(t, payload, "port", "unauthenticated callers must not learn the port")
			}
		})
	}
}

func TestStateChangingEndpointsStillRequireToken(t *testing.T) {
	handler := newCORSTestHandler(t, 1700)

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "download", method: http.MethodPost, path: "/download"},
		{name: "pause", method: http.MethodPost, path: "/pause?id=abc"},
		{name: "delete", method: http.MethodDelete, path: "/delete?id=abc"},
		{name: "list", method: http.MethodGet, path: "/list"},
		{name: "events", method: http.MethodGet, path: "/events"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			// An allowed origin is not a credential.
			req.Header.Set("Origin", testExtensionOrigin)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			body, err := io.ReadAll(rec.Body)
			require.NoError(t, err)
			assert.Contains(t, string(body), "Unauthorized")
		})
	}
}
