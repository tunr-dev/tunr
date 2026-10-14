package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeBridge struct {
	requested []string
	closed    []string
}

func (f *fakeBridge) ListResources(context.Context) ([]BridgeResource, error) {
	return []BridgeResource{{Address: "acme/erp-db", Kind: "postgres", GrantStatus: "approved", GrantMode: "ro"}}, nil
}
func (f *fakeBridge) RequestAccess(_ context.Context, res, mode, ttl, reason string) (BridgeGrant, error) {
	f.requested = append(f.requested, res+"|"+mode+"|"+ttl+"|"+reason)
	return BridgeGrant{ID: "grt_1", Address: res, Mode: "ro", Status: "pending"}, nil
}
func (f *fakeBridge) AccessStatus(_ context.Context, id string) (BridgeGrant, error) {
	if id != "grt_1" {
		return BridgeGrant{}, errors.New("not_found: grant not found")
	}
	exp := time.Now().Add(time.Hour)
	return BridgeGrant{ID: id, Status: "approved", ExpiresAt: &exp}, nil
}
func (f *fakeBridge) OpenAccess(_ context.Context, res string, port int) (OpenedAccess, error) {
	return OpenedAccess{Resource: res, Kind: "tcp", LocalAddress: "127.0.0.1:15432"}, nil
}
func (f *fakeBridge) CloseAccess(res string) error { f.closed = append(f.closed, res); return nil }

// rpc runs one session of requests through the server and returns responses by id.
func rpc(t *testing.T, s func(in *strings.Reader, out *strings.Builder) *Server, reqs ...string) map[float64]map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(reqs, "\n") + "\n")
	var out strings.Builder
	srv := s(in, &out)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Serve(ctx)
	res := map[float64]map[string]any{}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			if id, ok := m["id"].(float64); ok {
				res[id] = m
			}
		}
	}
	return res
}

func call(id int, name string, args any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	return string(b)
}

func text(m map[string]any) (string, bool) {
	r, _ := m["result"].(map[string]any)
	c, _ := r["content"].([]any)
	if len(c) == 0 {
		return "", false
	}
	isErr, _ := r["isError"].(bool)
	return c[0].(map[string]any)["text"].(string), isErr
}

func TestBridgeToolsListedAndWorking(t *testing.T) {
	fb := &fakeBridge{}
	mk := func(in *strings.Reader, out *strings.Builder) *Server {
		return NewForTest(in, out).WithBridge(fb)
	}
	res := rpc(t, mk,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		call(2, "tunr_list_resources", map[string]any{}),
		call(3, "tunr_request_access", map[string]any{"resource": "acme/erp-db", "reason": "October close"}),
		call(4, "tunr_request_access", map[string]any{"resource": "acme/erp-db"}),
		call(5, "tunr_access_status", map[string]any{"grant_id": "grt_1"}),
		call(6, "tunr_access_status", map[string]any{"grant_id": "grt_x"}),
		call(7, "tunr_open_access", map[string]any{"resource": "acme/erp-db", "local_port": 80}),
		call(8, "tunr_open_access", map[string]any{"resource": "acme/erp-db"}),
		call(9, "tunr_close_access", map[string]any{"resource": "acme/erp-db"}),
	)

	tools := res[1]["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"tunr_deploy", "tunr_share", "tunr_list_resources", "tunr_request_access",
		"tunr_access_status", "tunr_open_access", "tunr_close_access"} {
		if !names[want] {
			t.Errorf("tool %s missing", want)
		}
	}
	if len(tools) != 14 {
		t.Errorf("tool count = %d, want 14 (9 existing + 5 bridge)", len(tools))
	}

	cases := []struct {
		id      float64
		wantErr bool
		has     string
	}{
		{2, false, "acme/erp-db"},
		{3, false, `"status": "pending"`},
		{4, true, "reason are required"},
		{5, false, `"approved"`},
		{6, true, "not_found"},
		{7, true, "invalid local_port"},
		{8, false, "127.0.0.1:15432"},
		{9, false, "closed acme/erp-db"},
	}
	for _, c := range cases {
		txt, isErr := text(res[c.id])
		if isErr != c.wantErr || !strings.Contains(txt, c.has) {
			t.Errorf("call %v: isErr=%v text=%q, want err=%v containing %q", c.id, isErr, txt, c.wantErr, c.has)
		}
	}
	if len(fb.requested) != 1 || fb.requested[0] != "acme/erp-db||1h|October close" {
		t.Errorf("requests = %v (default ttl 1h)", fb.requested)
	}
	if len(fb.closed) != 1 {
		t.Errorf("closed = %v", fb.closed)
	}
}

func TestBridgeToolsWithoutToken(t *testing.T) {
	res := rpc(t, func(in *strings.Reader, out *strings.Builder) *Server { return NewForTest(in, out) },
		call(1, "tunr_list_resources", map[string]any{}))
	txt, isErr := text(res[1])
	if !isErr || !strings.Contains(txt, "TUNR_AGENT_TOKEN") {
		t.Errorf("unconfigured bridge: %q", txt)
	}
}

// The tool descriptions carry the read-only rule (roadmap §9).
func TestBridgeToolDescriptions(t *testing.T) {
	for _, tl := range bridgeToolList() {
		d := tl["description"].(string)
		if tl["name"] != "tunr_access_status" && tl["name"] != "tunr_close_access" && !strings.Contains(d, "read-only") {
			t.Errorf("%s description lacks the read-only rule", tl["name"])
		}
	}
}
