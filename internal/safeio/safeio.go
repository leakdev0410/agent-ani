// Package safeio contains bounded I/O helpers. All limits are checked with an
// extra byte so callers can distinguish an exact-limit body from a truncated
// one without trusting Content-Length.
package safeio

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

var ErrBodyTooLarge = errors.New("body exceeds configured limit")

type TooLargeError struct {
	Kind  string
	Limit int64
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%s: %v (%d bytes)", e.Kind, ErrBodyTooLarge, e.Limit)
}

func (e *TooLargeError) Unwrap() error { return ErrBodyTooLarge }

func ReadAll(r io.Reader, contentLength, limit int64, kind string) ([]byte, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%s: invalid byte limit %d", kind, limit)
	}
	if contentLength > limit {
		return nil, &TooLargeError{Kind: kind, Limit: limit}
	}
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, &TooLargeError{Kind: kind, Limit: limit}
	}
	return raw, nil
}

func ReadResponse(resp *http.Response, limit int64, kind string) ([]byte, error) {
	return ReadAll(resp.Body, resp.ContentLength, limit, kind)
}

func Preview(raw []byte, limit int64) string {
	if limit <= 0 || int64(len(raw)) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "…"
}
