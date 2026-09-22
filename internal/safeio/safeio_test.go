package safeio

import (
	"errors"
	"strings"
	"testing"
)

func TestReadAllBoundary(t *testing.T) {
	raw, err := ReadAll(strings.NewReader("1234"), -1, 4, "test")
	if err != nil || string(raw) != "1234" {
		t.Fatalf("exact limit: %q %v", raw, err)
	}
	_, err = ReadAll(strings.NewReader("12345"), -1, 4, "test")
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("want ErrBodyTooLarge, got %v", err)
	}
}
func TestReadAllRejectsContentLength(t *testing.T) {
	_, err := ReadAll(strings.NewReader("x"), 10, 4, "test")
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("got %v", err)
	}
}
