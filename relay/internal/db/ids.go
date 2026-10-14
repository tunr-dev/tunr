package db

import (
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// newID returns a prefixed random identifier ("org_3f2a9c0e1b7d4a65").
// 64 bits of entropy: these IDs are row keys, not secrets — anything that must
// not be guessable (site tokens, agent JWTs) is minted elsewhere.
func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// ErrNotFound is returned when a row does not exist *or is not visible to the
// calling org*. The two cases are deliberately indistinguishable: telling a
// caller "exists but not yours" leaks other tenants' IDs.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on a unique-constraint violation (duplicate name/slug).
var ErrConflict = errors.New("already exists")

// mapErr folds pgx's no-rows and unique-violation errors into the sentinels
// above so callers can map them to 404/409 without importing pgx.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}
