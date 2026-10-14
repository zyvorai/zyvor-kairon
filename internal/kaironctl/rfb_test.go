// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

// fakeRFBServer plays QEMU's side of the handshake and sends a 3x2 frame
// in two rectangles, with a Bell in between.
func fakeRFBServer(t *testing.T, c net.Conn) {
	t.Helper()
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte("RFB 003.008\n"))
	ver := make([]byte, 12)
	if _, err := io.ReadFull(c, ver); err != nil || string(ver) != "RFB 003.008\n" {
		t.Errorf("client version %q %v", ver, err)
		return
	}
	_, _ = c.Write([]byte{1, 1}) // one security type: None
	sel := make([]byte, 1)
	_, _ = io.ReadFull(c, sel)
	_ = binary.Write(c, binary.BigEndian, uint32(0)) // SecurityResult OK
	_, _ = io.ReadFull(c, sel)                       // ClientInit
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, []uint16{3, 2})
	b.Write(make([]byte, 16))
	_ = binary.Write(&b, binary.BigEndian, uint32(4))
	b.WriteString("qemu")
	_, _ = c.Write(b.Bytes())
	req := make([]byte, 20+8+10) // SetPixelFormat, SetEncodings, update request
	if _, err := io.ReadFull(c, req); err != nil {
		t.Errorf("read client requests: %v", err)
		return
	}
	pixel := func(r, g, bl byte) []byte { return []byte{bl, g, r, 0} }
	b.Reset()
	b.Write([]byte{0, 0, 0, 1})
	_ = binary.Write(&b, binary.BigEndian, []uint16{0, 0, 3, 1})
	_ = binary.Write(&b, binary.BigEndian, int32(0))
	b.Write(pixel(255, 0, 0))
	b.Write(pixel(0, 255, 0))
	b.Write(pixel(0, 0, 255))
	b.WriteByte(2) // Bell
	b.Write([]byte{0, 0, 0, 1})
	_ = binary.Write(&b, binary.BigEndian, []uint16{0, 1, 3, 1})
	_ = binary.Write(&b, binary.BigEndian, int32(0))
	for i := 0; i < 3; i++ {
		b.Write(pixel(10, 20, 30))
	}
	_, _ = c.Write(b.Bytes())
}

func TestRFBScreenshot(t *testing.T) {
	client, server := net.Pipe()
	go fakeRFBServer(t, server)
	img, err := rfbScreenshot(client)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 3 || b.Dy() != 2 {
		t.Fatalf("bounds %v", b)
	}
	for _, tc := range []struct {
		x, y    int
		r, g, b uint8
	}{{0, 0, 255, 0, 0}, {1, 0, 0, 255, 0}, {2, 0, 0, 0, 255}, {1, 1, 10, 20, 30}} {
		c := img.RGBAAt(tc.x, tc.y)
		if c.R != tc.r || c.G != tc.g || c.B != tc.b || c.A != 255 {
			t.Fatalf("pixel (%d,%d) = %v", tc.x, tc.y, c)
		}
	}
}

func TestRFBScreenshotRejectsPasswordDisplay(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		_, _ = server.Write([]byte("RFB 003.008\n"))
		_, _ = io.ReadFull(server, make([]byte, 12))
		_, _ = server.Write([]byte{1, 2}) // VNC auth only
	}()
	if _, err := rfbScreenshot(client); err == nil {
		t.Fatal("expected an authentication error")
	}
}
