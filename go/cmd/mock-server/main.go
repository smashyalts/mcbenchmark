// mock-server is a development aid: a minimal offline-mode server that brings
// replay sessions to play state (login -> config -> sync position), answers
// keep-alives, and drains serverbound packets. It implements no world logic; it
// exists only to smoke-test the mc-replay binary without a real Paper server.
package main

import (
	"flag"
	"log"
	"net"
	"sync/atomic"
	"time"

	"mcbench/internal/mcproto"
	"mcbench/internal/mcwire"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:25565", "listen address")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mock-server listening on %s", *addr)
	var conns int64
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		n := atomic.AddInt64(&conns, 1)
		go handle(c, n)
	}
}

func handle(conn net.Conn, id int64) {
	defer conn.Close()
	c := mcproto.NewCodec(conn)

	// Handshake + login start.
	if _, _, err := c.ReadPacket(); err != nil {
		return
	}
	_, body, err := c.ReadPacket()
	if err != nil {
		return
	}
	name, _ := mcwire.NewReader(body).String()

	c.WritePacket(mcproto.CBLoginSetCompression, mcwire.AppendVarInt(nil, 64))
	c.EnableCompression(64)

	success := make([]byte, 16)
	success = appendString(success, name)
	success = append(success, 0)
	c.WritePacket(mcproto.CBLoginSuccess, success)
	if _, _, err := c.ReadPacket(); err != nil { // login ack
		return
	}

	// Configuration: finish immediately.
	c.WritePacket(mcproto.CBConfigFinish, nil)
	if _, _, err := c.ReadPacket(); err != nil { // finish ack
		return
	}

	// Play: send sync position.
	sp := mcwire.AppendVarInt(nil, 1)
	w := mcwire.NewWriter()
	w.Float64BE(0)
	w.Float64BE(64)
	w.Float64BE(0)
	w.Float64BE(0)
	w.Float64BE(0)
	w.Float64BE(0)
	w.Float32BE(0)
	w.Float32BE(0)
	w.Int32BE(0)
	sp = append(sp, w.Bytes()...)
	c.WritePacket(mcproto.CBPlaySyncPosition, sp)
	log.Printf("conn %d (%s) reached play state", id, name)

	// Periodic keep-alives; drain everything the client sends.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		var kid int64 = 1
		for range t.C {
			if err := c.WritePacket(mcproto.CBPlayKeepAlive, mcproto.KeepAlive(kid)); err != nil {
				return
			}
			kid++
		}
	}()
	for {
		if _, _, err := c.ReadPacket(); err != nil {
			return
		}
	}
}

func appendString(dst []byte, s string) []byte {
	dst = mcwire.AppendVarInt(dst, int32(len(s)))
	return append(dst, s...)
}
