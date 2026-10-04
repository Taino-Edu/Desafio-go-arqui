package app

import (
	"encoding/base64"
	"errors"
	"testing"
)

func TestCursorRoundTrip(t *testing.T) {
	for _, v := range []int64{1, 2, 50, 1 << 40} {
		got, err := decodeCursor(encodeCursor(v))
		if err != nil || got != v {
			t.Errorf("round-trip %d = %d, %v", v, got, err)
		}
	}
	if v, err := decodeCursor(""); err != nil || v != 0 {
		t.Errorf("cursor vazio = %d, %v", v, err)
	}
}

func TestCursorInvalid(t *testing.T) {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, c := range []string{"!!!", b64("42"), b64("v2:5"), b64("v1:abc"), b64("v1:0"), b64("v1:-3")} {
		if _, err := decodeCursor(c); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("cursor %q: err = %v", c, err)
		}
	}
}
