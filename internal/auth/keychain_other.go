//go:build !darwin

package auth

import "errors"

func macKeychainAdd(_, _, _ string) ([]byte, error) { return nil, errors.New("not macOS") }
