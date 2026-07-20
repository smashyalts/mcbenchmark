package mcwire

import "testing"

// TestNegativeLengthDoesNotPanic guards the whole family of length-prefixed
// reads (event bodies, payloads, trace data), which all take their length from a
// VarInt and so can see a negative one from a truncated or corrupt file.
//
// Before the check in need(), that panicked with "slice bounds out of range"
// instead of reporting a bad file — and truncated captures are routine, since a
// server killed mid-write leaves one.
func TestNegativeLengthDoesNotPanic(t *testing.T) {
	// A VarInt encoding of -1 (five bytes), as a corrupt length prefix would be.
	r := NewReader([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0x0F, 0xAA, 0xBB})
	n, err := r.VarInt()
	if err != nil {
		t.Fatalf("varint: %v", err)
	}
	t.Logf("decoded length = %d", n)
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("PANIC on negative length: %v", p)
		}
	}()
	if _, err := r.Bytes(int(n)); err == nil {
		t.Errorf("Bytes(%d) returned no error for a negative length", n)
	}
}
