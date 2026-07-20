package com.mcbench.capture.io;

import java.nio.charset.StandardCharsets;

/**
 * ByteWriter mirrors the Go {@code mcwire.Writer} exactly. Every method here has
 * a byte-for-byte counterpart on the Go side; see docs/FORMAT.md.
 *
 * Integer VarInt/VarLong use Minecraft's LEB128 encoding (negative values take
 * the full 5/10 bytes). Multi-byte integers are big-endian. RawEvent payload
 * floats are little-endian; protocol-style floats (unused here) are big-endian.
 *
 * <h3>Why a raw array rather than ByteArrayOutputStream</h3>
 * This is on the writer thread, which encodes every captured event. It backs
 * onto a plain {@code byte[]} that {@link #reset} reuses across flushes, and
 * exposes {@link #array}/{@link #length} so callers can compress straight out of
 * it. The previous version wrapped a {@code ByteArrayOutputStream} and every
 * caller ended with {@code toByteArray()}, which copies.
 *
 * That mattered more than it looks. The writer thread's garbage is not free just
 * because it is off the main thread: a young GC is stop-the-world, so it pauses
 * the tick too. Measured at 30,000 events/sec, the old allocating path caused 7
 * young collections in 25 seconds with a 9.65 ms worst pause — 19% of a tick.
 *
 * Not thread-safe; one instance per writer thread.
 */
public final class ByteWriter {
    private byte[] buf;
    private int len;

    public ByteWriter() {
        this(64);
    }

    public ByteWriter(int initialCapacity) {
        this.buf = new byte[Math.max(16, initialCapacity)];
    }

    /** Discards contents, keeping the buffer for reuse. */
    public void reset() { len = 0; }

    /** The backing array. Only the first {@link #length} bytes are valid. */
    public byte[] array() { return buf; }

    public int length() { return len; }

    public int size() { return len; }

    /** Copies out the written bytes. Avoid on hot paths; prefer array()/length(). */
    public byte[] toByteArray() {
        byte[] out = new byte[len];
        System.arraycopy(buf, 0, out, 0, len);
        return out;
    }

    private void ensure(int extra) {
        int need = len + extra;
        if (need <= buf.length) {
            return;
        }
        int cap = buf.length;
        while (cap < need) {
            cap <<= 1;
        }
        byte[] next = new byte[cap];
        System.arraycopy(buf, 0, next, 0, len);
        buf = next;
    }

    public void raw(byte[] b) { raw(b, 0, b.length); }

    public void raw(byte[] b, int off, int n) {
        ensure(n);
        System.arraycopy(b, off, buf, len, n);
        len += n;
    }

    public void u8(int v) {
        ensure(1);
        buf[len++] = (byte) v;
    }

    public void bool(boolean v) { u8(v ? 1 : 0); }

    public void varInt(int value) {
        ensure(5);
        int v = value;
        while (true) {
            if ((v & ~0x7F) == 0) {
                buf[len++] = (byte) v;
                return;
            }
            buf[len++] = (byte) ((v & 0x7F) | 0x80);
            v >>>= 7;
        }
    }

    /** Encoded size of a VarInt, so a length prefix can be written before the body. */
    public static int varIntSize(int value) {
        int v = value;
        int n = 1;
        while ((v & ~0x7F) != 0) {
            v >>>= 7;
            n++;
        }
        return n;
    }

    public void varLong(long value) {
        ensure(10);
        long v = value;
        while (true) {
            if ((v & ~0x7FL) == 0) {
                buf[len++] = (byte) v;
                return;
            }
            buf[len++] = (byte) ((v & 0x7F) | 0x80);
            v >>>= 7;
        }
    }

    public void int64BE(long v) {
        ensure(8);
        for (int i = 56; i >= 0; i -= 8) {
            buf[len++] = (byte) ((v >>> i) & 0xFF);
        }
    }

    public void int32BE(int v) {
        ensure(4);
        for (int i = 24; i >= 0; i -= 8) {
            buf[len++] = (byte) ((v >>> i) & 0xFF);
        }
    }

    public void uint16BE(int v) {
        ensure(2);
        buf[len++] = (byte) ((v >>> 8) & 0xFF);
        buf[len++] = (byte) (v & 0xFF);
    }

    /** Little-endian float32, used by RawEvent payloads. */
    public void float32LE(float f) {
        ensure(4);
        int bits = Float.floatToIntBits(f);
        buf[len++] = (byte) (bits & 0xFF);
        buf[len++] = (byte) ((bits >>> 8) & 0xFF);
        buf[len++] = (byte) ((bits >>> 16) & 0xFF);
        buf[len++] = (byte) ((bits >>> 24) & 0xFF);
    }

    /** VarInt length prefix followed by UTF-8 bytes. */
    public void string(String s) {
        byte[] b = s.getBytes(StandardCharsets.UTF_8);
        varInt(b.length);
        raw(b);
    }

    /** Pre-encoded string: VarInt length prefix plus the given UTF-8 bytes. */
    public void stringBytes(byte[] utf8) {
        varInt(utf8.length);
        raw(utf8);
    }
}
