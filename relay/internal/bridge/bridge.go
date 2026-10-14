package bridge

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tunr-dev/tunr/relay/internal/auth"
	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/logger"
)

// EnvEnabled is the feature flag. Only "1" or "true" turn the bridge on; any
// other value — including a typo — leaves it off, because off is the state
// that is known to be safe.
const EnvEnabled = "TUNR_BRIDGE_ENABLED"

// EnvPublicURL is the relay URL connectors and agents dial (shown in the
// connect command). Defaults to https://relay.<TUNR_DOMAIN>.
const EnvPublicURL = "TUNR_BRIDGE_PUBLIC_URL"

// Config holds the bridge settings read at boot.
type Config struct {
	Enabled    bool
	SigningKey string // TUNR_GRANT_SIGNING_KEY
	PublicURL  string
	Issuer     string // grant JWT iss; the relay domain
	DevMode    bool   // allows an ephemeral signing key
}

// ConfigFromEnv reads the bridge config through getenv (os.Getenv in main,
// a map in tests).
func ConfigFromEnv(getenv func(string) string) Config {
	flag := func(k string) bool {
		v := strings.TrimSpace(strings.ToLower(getenv(k)))
		return v == "1" || v == "true"
	}
	domain := getenv("TUNR_DOMAIN")
	if domain == "" {
		domain = "tunr.sh"
	}
	public := getenv(EnvPublicURL)
	if public == "" {
		public = "https://relay." + domain
	}
	return Config{
		Enabled:    flag(EnvEnabled),
		SigningKey: getenv(auth.EnvGrantSigningKey),
		PublicURL:  strings.TrimRight(public, "/"),
		Issuer:     domain,
		DevMode:    flag("TUNR_DEV_MODE"),
	}
}

// Deps are the relay components the bridge uses.
type Deps struct {
	DB  *db.DB
	JWT *auth.JWTAuth
}

// Server is the bridge's HTTP surface.
type Server struct {
	cfg    Config
	db     *db.DB
	jwt    *auth.JWTAuth
	signer *auth.GrantSigner
	hub    *ConnectorHub
	access *Access
}

// New builds the bridge server. With cfg.Enabled false it does nothing and
// cannot fail, so a disabled bridge can never take the relay down.
func New(cfg Config, deps Deps) (*Server, error) {
	s := &Server{cfg: cfg, db: deps.DB, jwt: deps.JWT}
	if !cfg.Enabled {
		return s, nil
	}
	if deps.DB == nil {
		return nil, errors.New(EnvEnabled + " requires DATABASE_URL (the bridge has no in-memory mode)")
	}
	if deps.JWT == nil {
		return nil, errors.New("bridge: JWT auth is required")
	}
	switch {
	case cfg.SigningKey != "":
		signer, err := auth.NewGrantSigner(cfg.Issuer, cfg.SigningKey)
		if err != nil {
			return nil, err
		}
		s.signer = signer
	case cfg.DevMode:
		signer, _, err := auth.GenerateGrantSigner(cfg.Issuer)
		if err != nil {
			return nil, err
		}
		s.signer = signer
		logger.Warn("bridge: %s unset — using an EPHEMERAL grant key (dev mode). "+
			"Connectors pinned to it break on restart.", auth.EnvGrantSigningKey)
	default:
		return nil, errors.New(auth.EnvGrantSigningKey + " is required when " + EnvEnabled +
			"=1 (connectors pin the relay key; an ephemeral key would break them on every restart)")
	}
	s.hub = NewConnectorHub(deps.DB, s.signer.KeyString())
	s.access = NewAccess(deps.DB, deps.JWT, s.signer, s.hub)
	return s, nil
}

// Hub exposes the connector hub (nil when disabled).
func (s *Server) Hub() *ConnectorHub { return s.hub }

// Enabled reports whether the flag was on at boot.
func (s *Server) Enabled() bool { return s.cfg.Enabled }

// RelayKey is the printable grant-signing public key ("" when disabled).
func (s *Server) RelayKey() string {
	if s.signer == nil {
		return ""
	}
	return s.signer.KeyString()
}

// RegisterRoutes mounts the bridge endpoints when enabled. When disabled it
// mounts nothing, so /bridge/* and /api/v1/bridge/* keep falling through to
// the tunnel proxy exactly as before.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	if !s.Enabled() {
		return
	}
	mux.HandleFunc("GET /api/v1/bridge/health", s.handleHealth)
	mux.HandleFunc("GET /bridge/connect", s.hub.ServeConnect)
	mux.HandleFunc("GET /bridge/access", s.access.ServeAccess)
	mux.HandleFunc("GET /bridge/access/check", s.access.ServeCheck)
	s.registerAPI(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "relay_key": s.RelayKey()})
}
