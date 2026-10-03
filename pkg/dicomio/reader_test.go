package dicomio

import (
	"bufio"
	"bytes"
	"io"
	"testing"
)

// TestNestedPushLimitReadUntilEOF verifies that nested PushLimit/PopLimit
// pairs behave correctly when the outermost reader is configured to read until
// EOF (limit == LimitReadUntilEOF). In particular an inner defined-length item
// fully enclosed in an outer defined-length item must not consume the bytes
// that follow the outer item, and after popping both limits the remaining
// stream must still be readable.
func TestNestedPushLimitReadUntilEOF(t *testing.T) {
	data := bytes.Repeat([]byte{0xaa}, 100)
	r := NewReader(bufio.NewReader(bytes.NewReader(data)), nil, LimitReadUntilEOF)

	// Outer item spans 50 bytes starting at offset 0.
	if err := r.PushLimit(50); err != nil {
		t.Fatalf("outer push: %v", err)
	}
	if _, err := io.ReadFull(r, make([]byte, 10)); err != nil {
		t.Fatalf("read 10: %v", err)
	}

	// Inner item spans 20 bytes starting at offset 10 (absolute end 30).
	if err := r.PushLimit(20); err != nil {
		t.Fatalf("inner push: %v", err)
	}
	if _, err := io.ReadFull(r, make([]byte, 20)); err != nil {
		t.Fatalf("read inner: %v", err)
	}
	if left := r.BytesLeftUntilLimit(); left != 0 {
		t.Fatalf("inner limit left=%d, want 0", left)
	}
	r.PopLimit()

	// After popping the inner limit the outer limit must still have 20 bytes.
	if left := r.BytesLeftUntilLimit(); left != 20 {
		t.Fatalf("outer limit left=%d after inner pop, want 20", left)
	}
	if _, err := io.ReadFull(r, make([]byte, 20)); err != nil {
		t.Fatalf("read outer remainder: %v", err)
	}
	r.PopLimit()

	// After the outer item, the remaining 50 bytes (up to EOF) must remain.
	if _, err := io.ReadFull(r, make([]byte, 50)); err != nil {
		t.Fatalf("read post-item stream: %v", err)
	}
}

// TestPushLimitExceedsEnclosing verifies that an inner limit cannot extend
// past an enclosing concrete limit, but still works under read-until-EOF.
func TestPushLimitExceedsEnclosing(t *testing.T) {
	r := NewReader(bufio.NewReader(bytes.NewReader(make([]byte, 100))), nil, 40)
	if err := r.PushLimit(50); err == nil {
		r.PopLimit()
		t.Fatal("expected error pushing limit past concrete enclosing limit")
	}

	r2 := NewReader(bufio.NewReader(bytes.NewReader(make([]byte, 100))), nil, LimitReadUntilEOF)
	if err := r2.PushLimit(50); err != nil {
		t.Fatalf("push under read-until-EOF should succeed: %v", err)
	}
	r2.PopLimit()
}
