// Package mcwire implements the primitive binary encodings shared by the
// RawEvent log format, the trace file format, and the Minecraft protocol:
// Minecraft VarInt/VarLong, big-endian integers, and little-endian floats.
//
// The encoding rules mirror the Java side (capture-plugin ByteWriter) exactly.
// See docs/FORMAT.md.
package mcwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

var ErrVarIntTooLong = errors.New("mcwire: varint too long")

// Writer accumulates bytes in memory.
type Writer struct {
	buf []byte
}

func NewWriter() *Writer { return &Writer{buf: make([]byte, 0, 64)} }

func (w *Writer) Bytes() []byte { return w.buf }

// Reset discards contents but keeps the buffer, so a caller encoding many
// values in a loop allocates once rather than once per value.
func (w *Writer) Reset()   { w.buf = w.buf[:0] }
func (w *Writer) Len() int { return len(w.buf) }

func (w *Writer) Raw(b []byte) { w.buf = append(w.buf, b...) }
func (w *Writer) Byte(b byte)  { w.buf = append(w.buf, b) }
func (w *Writer) Bool(v bool) {
	if v {
		w.Byte(1)
	} else {
		w.Byte(0)
	}
}

// VarInt writes a Minecraft VarInt (unsigned LEB128 of the two's-complement
// 32-bit value; negative numbers always take 5 bytes).
func (w *Writer) VarInt(v int32) {
	u := uint32(v)
	for {
		if u&^0x7F == 0 {
			w.buf = append(w.buf, byte(u))
			return
		}
		w.buf = append(w.buf, byte(u&0x7F|0x80))
		u >>= 7
	}
}

// VarLong writes a Minecraft VarLong (64-bit analogue of VarInt).
func (w *Writer) VarLong(v int64) {
	u := uint64(v)
	for {
		if u&^0x7F == 0 {
			w.buf = append(w.buf, byte(u))
			return
		}
		w.buf = append(w.buf, byte(u&0x7F|0x80))
		u >>= 7
	}
}

func (w *Writer) Int64BE(v int64) {
	w.buf = binary.BigEndian.AppendUint64(w.buf, uint64(v))
}

func (w *Writer) Int32BE(v int32) {
	w.buf = binary.BigEndian.AppendUint32(w.buf, uint32(v))
}

func (w *Writer) Uint16BE(v uint16) {
	w.buf = binary.BigEndian.AppendUint16(w.buf, v)
}

func (w *Writer) Float32BE(v float32) {
	w.buf = binary.BigEndian.AppendUint32(w.buf, math.Float32bits(v))
}

func (w *Writer) Float64BE(v float64) {
	w.buf = binary.BigEndian.AppendUint64(w.buf, math.Float64bits(v))
}

// Float32LE is used by RawEvent payloads (spec: floats little-endian).
func (w *Writer) Float32LE(v float32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, math.Float32bits(v))
}

// String writes a VarInt length prefix followed by UTF-8 bytes.
func (w *Writer) String(s string) {
	w.VarInt(int32(len(s)))
	w.buf = append(w.buf, s...)
}

// Reader consumes bytes from an in-memory slice.
type Reader struct {
	data []byte
	off  int
}

func NewReader(data []byte) *Reader { return &Reader{data: data} }

func (r *Reader) Remaining() int { return len(r.data) - r.off }
func (r *Reader) Rest() []byte   { return r.data[r.off:] }

func (r *Reader) need(n int) error {
	// Reject negative lengths here rather than at each call site. Every
	// length-prefixed field (event bodies, payloads, trace data) gets its length
	// from a VarInt, and a VarInt decodes to a signed int32 — so a truncated or
	// corrupt file yields a negative length, and `data[off : off+n]` panics with
	// "slice bounds out of range" instead of reporting a bad file. Truncated
	// captures are routine: a server killed mid-write leaves one.
	if n < 0 {
		return fmt.Errorf("mcwire: negative length %d", n)
	}
	if r.Remaining() < n {
		return fmt.Errorf("mcwire: need %d bytes, have %d", n, r.Remaining())
	}
	return nil
}

func (r *Reader) Byte() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	b := r.data[r.off]
	r.off++
	return b, nil
}

func (r *Reader) Bool() (bool, error) {
	b, err := r.Byte()
	return b != 0, err
}

func (r *Reader) Bytes(n int) ([]byte, error) {
	if err := r.need(n); err != nil {
		return nil, err
	}
	b := r.data[r.off : r.off+n]
	r.off += n
	return b, nil
}

func (r *Reader) VarInt() (int32, error) {
	var u uint32
	for i := 0; i < 5; i++ {
		b, err := r.Byte()
		if err != nil {
			return 0, err
		}
		u |= uint32(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return int32(u), nil
		}
	}
	return 0, ErrVarIntTooLong
}

func (r *Reader) VarLong() (int64, error) {
	var u uint64
	for i := 0; i < 10; i++ {
		b, err := r.Byte()
		if err != nil {
			return 0, err
		}
		u |= uint64(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return int64(u), nil
		}
	}
	return 0, ErrVarIntTooLong
}

func (r *Reader) Int64BE() (int64, error) {
	b, err := r.Bytes(8)
	if err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

func (r *Reader) Int32BE() (int32, error) {
	b, err := r.Bytes(4)
	if err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(b)), nil
}

func (r *Reader) Float32BE() (float32, error) {
	b, err := r.Bytes(4)
	if err != nil {
		return 0, err
	}
	return math.Float32frombits(binary.BigEndian.Uint32(b)), nil
}

func (r *Reader) Float64BE() (float64, error) {
	b, err := r.Bytes(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
}

func (r *Reader) Float32LE() (float32, error) {
	b, err := r.Bytes(4)
	if err != nil {
		return 0, err
	}
	return math.Float32frombits(binary.LittleEndian.Uint32(b)), nil
}

func (r *Reader) String() (string, error) {
	n, err := r.VarInt()
	if err != nil {
		return "", err
	}
	if n < 0 {
		return "", fmt.Errorf("mcwire: negative string length %d", n)
	}
	b, err := r.Bytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadVarIntFrom reads a VarInt from a byte stream (used for packet framing).
func ReadVarIntFrom(r io.ByteReader) (int32, error) {
	var u uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		u |= uint32(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return int32(u), nil
		}
	}
	return 0, ErrVarIntTooLong
}

// AppendVarInt appends a VarInt to dst and returns the extended slice.
func AppendVarInt(dst []byte, v int32) []byte {
	u := uint32(v)
	for {
		if u&^0x7F == 0 {
			return append(dst, byte(u))
		}
		dst = append(dst, byte(u&0x7F|0x80))
		u >>= 7
	}
}
