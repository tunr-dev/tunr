package access

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tunr-dev/tunr/internal/bridge/query"
)

// query.go — the agent side of query-mode resources (kind "mssql"): one
// long-lived stream that carries JSON queries to the customer's connector,
// and a local HTTP endpoint so any program can use it:
//
//	curl -s http://127.0.0.1:PORT/query -H "Authorization: Bearer $SECRET" \
//	     -d '{"sql":"SELECT TOP 5 * FROM cdItem"}'

// QuerySession runs queries over one stream, reopening it once if it broke
// (the connector restarted, the relay cycled). Safe for concurrent use;
// queries run one at a time.
type QuerySession struct {
	openStream func(context.Context) (*Stream, error)
	secret     string
	mu         sync.Mutex
	st         *Stream
	qc         *query.Conn
	seq        int
}

// NewQuerySession prepares a session; the stream opens on the first query.
func (c *Client) NewQuerySession() *QuerySession {
	return &QuerySession{openStream: c.Open, secret: c.ClientSecret}
}

// NewQuerySessionWith runs a session over streams from open, presenting
// secret (tests, and callers with their own transport).
func NewQuerySessionWith(open func(context.Context) (*Stream, error), secret string) *QuerySession {
	return &QuerySession{openStream: open, secret: secret}
}

func (q *QuerySession) open(ctx context.Context) error {
	st, err := q.openStream(ctx)
	if err != nil {
		return err
	}
	qc := query.NewConn(st)
	if dl, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(dl)
	}
	var h query.Hello
	err = qc.Write(query.Auth{Auth: q.secret})
	if err == nil {
		err = qc.Read(&h, 4096)
	}
	_ = st.SetDeadline(time.Time{})
	if err != nil {
		_ = st.Close()
		if r := st.Reason(); r != "" {
			return fmt.Errorf("stream refused by the customer's connector: %s", r)
		}
		return fmt.Errorf("query session: %w", err)
	}
	if h.Error != nil {
		_ = st.Close()
		return h.Error
	}
	q.st, q.qc = st, qc
	return nil
}

func (q *QuerySession) drop() {
	if q.st != nil {
		_ = q.st.Close()
	}
	q.st, q.qc = nil, nil
}

// Query runs one statement. A refusal or SQL error comes back in
// Response.Error; err is for transport failures.
func (q *QuerySession) Query(ctx context.Context, sql string, maxRows int) (*query.Response, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if q.qc == nil {
			if err := q.open(ctx); err != nil {
				return nil, err
			}
		}
		q.seq++
		id := strconv.Itoa(q.seq)
		if dl, ok := ctx.Deadline(); ok {
			_ = q.st.SetDeadline(dl)
		} else {
			_ = q.st.SetDeadline(time.Now().Add(2 * time.Minute))
		}
		var resp query.Response
		err := q.qc.Write(query.Request{ID: id, SQL: sql, MaxRows: maxRows})
		if err == nil {
			err = q.qc.Read(&resp, query.MaxResponse+4096)
		}
		if err == nil {
			_ = q.st.SetDeadline(time.Time{})
			if resp.Error != nil && resp.Error.Code == "data_limit" {
				q.drop() // the connector ends the session after this
			}
			return &resp, nil
		}
		reason := q.st.Reason()
		q.drop()
		// Retry once on a stream that died between queries — never after the
		// context ran out, and never twice (the statement may have run).
		if attempt > 0 || ctx.Err() != nil || reason == "killed" || reason == "revoked" || reason == "expired" {
			if reason != "" {
				return nil, fmt.Errorf("stream closed: %s", reason)
			}
			return nil, err
		}
	}
}

// Close ends the stream.
func (q *QuerySession) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.drop()
}

// QueryHandler serves POST /query for local programs. secret is required as
// a bearer token so other local users can't use the port.
func QueryHandler(q *QuerySession, secret string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("POST /query", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(secret)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(query.Response{Error: &query.Error{Code: "auth_failed",
				Message: "send the token printed by `tunr access` as Authorization: Bearer …"}})
			return
		}
		var in struct {
			SQL     string `json:"sql"`
			MaxRows int    `json:"max_rows"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, query.MaxRequest))
		if err := dec.Decode(&in); err != nil || strings.TrimSpace(in.SQL) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(query.Response{Error: &query.Error{Code: "bad_request", Message: `body must be {"sql": "SELECT …"}`}})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		resp, err := q.Query(ctx, in.SQL, in.MaxRows)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(query.Response{Error: &query.Error{Code: "bridge", Message: err.Error()}})
			return
		}
		if resp.Error != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}

// ServeQuery serves QueryHandler on l until ctx ends.
func ServeQuery(ctx context.Context, l net.Listener, q *QuerySession, secret string) error {
	srv := &http.Server{Handler: QueryHandler(q, secret), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		q.Close()
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
