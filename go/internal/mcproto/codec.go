package mcproto

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"net"
	"sync"

	"mcbench/internal/mcwire"
)

// MaxPacketLen guards against corrupt frames.
const MaxPacketLen = 8 << 20 // 8 MiB (registry/chunk packets can be large)

// Codec frames Minecraft packets over a connection, transparently handling
// the post-login zlib compression mode.
//
// ReadPacket must only be called from one goroutine; WritePacket is
// goroutine-safe (the reader goroutine and the trace sender both write).
type Codec struct {
	conn net.Conn
	r    *bufio.Reader

	wmu sync.Mutex

	// compression threshold; <0 means compression disabled
	threshold int
	tmu       sync.RWMutex

	BytesIn  int64
	BytesOut int64
}

func NewCodec(conn net.Conn) *Codec {
	return &Codec{conn: conn, r: bufio.NewReaderSize(conn, 64<<10), threshold: -1}
}

// EnableCompression switches the codec into compressed framing.
func (c *Codec) EnableCompression(threshold int) {
	c.tmu.Lock()
	c.threshold = threshold
	c.tmu.Unlock()
}

func (c *Codec) compressionThreshold() int {
	c.tmu.RLock()
	defer c.tmu.RUnlock()
	return c.threshold
}

// ReadPacket reads one packet, returning its ID and body.
func (c *Codec) ReadPacket() (int32, []byte, error) {
	length, err := mcwire.ReadVarIntFrom(c.r)
	if err != nil {
		return 0, nil, err
	}
	if length <= 0 || length > MaxPacketLen {
		return 0, nil, fmt.Errorf("mcproto: invalid packet length %d", length)
	}
	frame := make([]byte, length)
	if _, err := io.ReadFull(c.r, frame); err != nil {
		return 0, nil, err
	}
	c.BytesIn += int64(length)

	payload := frame
	if c.compressionThreshold() >= 0 {
		br := bytes.NewReader(frame)
		dataLen, err := mcwire.ReadVarIntFrom(br)
		if err != nil {
			return 0, nil, err
		}
		rest := frame[len(frame)-br.Len():]
		if dataLen == 0 {
			payload = rest
		} else {
			if dataLen > MaxPacketLen {
				return 0, nil, fmt.Errorf("mcproto: invalid uncompressed length %d", dataLen)
			}
			zr, err := zlib.NewReader(bytes.NewReader(rest))
			if err != nil {
				return 0, nil, fmt.Errorf("mcproto: zlib: %w", err)
			}
			out := make([]byte, dataLen)
			if _, err := io.ReadFull(zr, out); err != nil {
				return 0, nil, fmt.Errorf("mcproto: zlib body: %w", err)
			}
			zr.Close()
			payload = out
		}
	}

	pr := mcwire.NewReader(payload)
	id, err := pr.VarInt()
	if err != nil {
		return 0, nil, err
	}
	return id, pr.Rest(), nil
}

// WritePacket frames and sends one packet (id + body).
func (c *Codec) WritePacket(id int32, body []byte) error {
	inner := mcwire.AppendVarInt(nil, id)
	inner = append(inner, body...)

	var frame []byte
	if th := c.compressionThreshold(); th >= 0 {
		if len(inner) < th {
			data := mcwire.AppendVarInt(nil, 0)
			data = append(data, inner...)
			frame = mcwire.AppendVarInt(nil, int32(len(data)))
			frame = append(frame, data...)
		} else {
			var zbuf bytes.Buffer
			zw := zlib.NewWriter(&zbuf)
			if _, err := zw.Write(inner); err != nil {
				return err
			}
			if err := zw.Close(); err != nil {
				return err
			}
			data := mcwire.AppendVarInt(nil, int32(len(inner)))
			data = append(data, zbuf.Bytes()...)
			frame = mcwire.AppendVarInt(nil, int32(len(data)))
			frame = append(frame, data...)
		}
	} else {
		frame = mcwire.AppendVarInt(nil, int32(len(inner)))
		frame = append(frame, inner...)
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	n, err := c.conn.Write(frame)
	c.BytesOut += int64(n)
	return err
}

func (c *Codec) Close() error { return c.conn.Close() }
