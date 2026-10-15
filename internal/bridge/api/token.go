package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TokenSource yields an agent token: either a fixed one (TUNR_AGENT_TOKEN) or,
// inside a hosted agent, one exchanged from TUNR_AGENT_BOOTSTRAP and renewed
// five minutes before it expires. Safe for concurrent use; never logs tokens.
type TokenSource struct {
	Relay     string
	Static    string // used as-is when set
	Bootstrap string // "tunr_boot_…"
	HTTP      *http.Client

	mu  sync.Mutex
	tok string
	exp time.Time
}

const renewBefore = 5 * time.Minute

// Token returns a valid agent token.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	if s.Static != "" {
		return s.Static, nil
	}
	if s.Bootstrap == "" {
		return "", errors.New("no agent token: set TUNR_AGENT_TOKEN (or run inside a hosted agent)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != "" && time.Until(s.exp) > renewBefore {
		return s.tok, nil
	}
	body, _ := json.Marshal(map[string]string{"bootstrap": s.Bootstrap})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.Relay, "/")+"/api/v1/bridge/agent/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("agent token exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agent token exchange refused (%d) — the agent was redeployed or disabled", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", errors.New("agent token exchange: bad response")
	}
	s.tok, s.exp = out.Token, out.ExpiresAt
	return s.tok, nil
}
