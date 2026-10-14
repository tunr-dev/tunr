package bridge

import (
	"encoding/json"
	"net/http"
	"strings"
)

// EnvEnabled is the feature flag. Only "1" or "true" turn the bridge on; any
// other value — including a typo — leaves it off, because off is the state
// that is known to be safe.
const EnvEnabled = "TUNR_BRIDGE_ENABLED"

// Config holds the bridge settings read at boot.
type Config struct {
	Enabled bool
}

// ConfigFromEnv reads the bridge config through getenv (os.Getenv in main,
// a map in tests).
func ConfigFromEnv(getenv func(string) string) Config {
	v := strings.TrimSpace(strings.ToLower(getenv(EnvEnabled)))
	return Config{Enabled: v == "1" || v == "true"}
}

// Server is the bridge's HTTP surface.
type Server struct {
	cfg Config
}

// New builds the bridge server. It does no I/O; with cfg.Enabled false it is
// inert.
func New(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// Enabled reports whether the flag was on at boot.
func (s *Server) Enabled() bool { return s.cfg.Enabled }

// RegisterRoutes mounts the bridge endpoints when enabled. When disabled it
// mounts nothing, so /bridge/* and /api/v1/bridge/* keep falling through to
// the tunnel proxy exactly as before.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	if !s.Enabled() {
		return
	}
	mux.HandleFunc("/api/v1/bridge/health", s.handleHealth)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
