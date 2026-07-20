package mcwire

import "testing"

func TestVarIntRoundTrip(t *testing.T) {
	cases := []int32{0, 1, 2, 127, 128, 255, 256, 25565, -1, -128, 2147483647, -2147483648}
	for _, v := range cases {
		w := NewWriter()
		w.VarInt(v)
		got, err := NewReader(w.Bytes()).VarInt()
		if err != nil {
			t.Fatalf("VarInt(%d): %v", v, err)
		}
		if got != v {
			t.Errorf("VarInt round trip: got %d want %d", got, v)
		}
	}
}

// Known-answer vectors from the Minecraft protocol spec.
func TestVarIntGolden(t *testing.T) {
	golden := map[int32][]byte{
		0:          {0x00},
		1:          {0x01},
		127:        {0x7f},
		128:        {0x80, 0x01},
		255:        {0xff, 0x01},
		25565:      {0xdd, 0xc7, 0x01},
		2147483647: {0xff, 0xff, 0xff, 0xff, 0x07},
		-1:         {0xff, 0xff, 0xff, 0xff, 0x0f},
	}
	for v, want := range golden {
		w := NewWriter()
		w.VarInt(v)
		got := w.Bytes()
		if len(got) != len(want) {
			t.Fatalf("VarInt(%d): got %x want %x", v, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("VarInt(%d): got %x want %x", v, got, want)
			}
		}
	}
}

func TestVarLongRoundTrip(t *testing.T) {
	cases := []int64{0, 1, 9223372036854775807, -1, -9223372036854775808, 1234567890123}
	for _, v := range cases {
		w := NewWriter()
		w.VarLong(v)
		got, err := NewReader(w.Bytes()).VarLong()
		if err != nil {
			t.Fatalf("VarLong(%d): %v", v, err)
		}
		if got != v {
			t.Errorf("VarLong round trip: got %d want %d", got, v)
		}
	}
}

func TestStringAndFloat(t *testing.T) {
	w := NewWriter()
	w.String("hello arena")
	w.Float32LE(3.5)
	w.Float64BE(-1.25)
	r := NewReader(w.Bytes())
	s, err := r.String()
	if err != nil || s != "hello arena" {
		t.Fatalf("String: %q %v", s, err)
	}
	f, err := r.Float32LE()
	if err != nil || f != 3.5 {
		t.Fatalf("Float32LE: %v %v", f, err)
	}
	d, err := r.Float64BE()
	if err != nil || d != -1.25 {
		t.Fatalf("Float64BE: %v %v", d, err)
	}
}
