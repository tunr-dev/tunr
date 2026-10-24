// Package query is the bridge's query-mode protocol: what flows inside one
// bridge stream when the resource is served by the connector running the
// SQL itself (kind "mssql") rather than by a wire-level proxy (postgres).
//
// It is newline-delimited JSON. The agent speaks first:
//
//	→ {"auth":"<client secret>"}                       once
//	← {"ok":true,"kind":"mssql"}                        or {"error":{…}} and close
//	→ {"id":"1","sql":"SELECT …","max_rows":100}
//	← {"id":"1","result_sets":[{"columns":[…],"rows":[[…]]}],"row_count":3,…}
//
// Requests are answered in order, one at a time. The client secret is the
// same per-process secret `tunr access` puts in a postgres DSN: only its
// SHA-256 crosses the relay, and the connector accepts only that secret.
package query

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	// MaxRequest bounds one request line (SQL text included).
	MaxRequest = 256 << 10
	// MaxResponse bounds one response line; larger results are truncated.
	MaxResponse = 8 << 20
)

// Auth is the agent's first line.
type Auth struct {
	Auth string `json:"auth"`
}

// Hello answers Auth.
type Hello struct {
	OK    bool   `json:"ok,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// Request is one query.
type Request struct {
	ID      string `json:"id"`
	SQL     string `json:"sql"`
	MaxRows int    `json:"max_rows,omitempty"` // may only narrow the grant's limit
}

// Column describes one result column.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ResultSet is one result of a statement batch.
type ResultSet struct {
	Columns []Column `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// Response answers one Request.
type Response struct {
	ID         string      `json:"id"`
	ResultSets []ResultSet `json:"result_sets,omitempty"`
	RowCount   int         `json:"row_count"`
	Truncated  bool        `json:"truncated,omitempty"`
	Notice     string      `json:"notice,omitempty"`
	DurationMS int         `json:"duration_ms"`
	Error      *Error      `json:"error,omitempty"`
}

// Error is a refusal or a database error.
type Error struct {
	Code    string `json:"code"` // blocked | rate_limited | data_limit | db_error | bad_request | auth_failed | upstream
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Conn frames JSON lines over a stream. Reads and writes are each serialised.
type Conn struct {
	rw  io.ReadWriter
	r   *bufio.Reader
	rmu sync.Mutex
	wmu sync.Mutex
}

// NewConn wraps rw.
func NewConn(rw io.ReadWriter) *Conn {
	return &Conn{rw: rw, r: bufio.NewReaderSize(rw, 64<<10)}
}

// ErrTooLarge is returned for a line over the limit.
var ErrTooLarge = errors.New("query: message too large")

// Read decodes the next line into v; lines longer than limit are refused.
func (c *Conn) Read(v any, limit int) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	var line []byte
	for {
		chunk, isPrefix, err := c.r.ReadLine()
		if err != nil {
			return err
		}
		if len(line)+len(chunk) > limit {
			return ErrTooLarge
		}
		line = append(line, chunk...)
		if !isPrefix {
			break
		}
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber() // keep bigint values exact
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("query: bad message: %w", err)
	}
	return nil
}

// Write encodes v as one line.
func (c *Conn) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.rw.Write(append(b, '\n'))
	return err
}
