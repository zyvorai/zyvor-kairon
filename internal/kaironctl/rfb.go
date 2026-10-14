// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"image"
	"io"
)

// rfbScreenshot reads one full frame from an RFB (VNC) server: version
// 3.3-3.8 handshake with security type None, a 32-bit true-colour pixel
// format, Raw encoding and a single non-incremental update request. That
// is all QEMU's VNC server needs; it is not a general VNC client.
func rfbScreenshot(conn io.ReadWriter) (*image.RGBA, error) {
	r := bufio.NewReader(conn)
	var ver [12]byte
	if _, err := io.ReadFull(r, ver[:]); err != nil {
		return nil, fmt.Errorf("rfb: read version: %w", err)
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(ver[:]), "RFB %03d.%03d\n", &major, &minor); err != nil || major != 3 {
		return nil, fmt.Errorf("rfb: unsupported server version %q", ver[:])
	}
	if minor > 8 {
		minor = 8
	}
	if minor < 7 {
		minor = 3
	}
	if _, err := fmt.Fprintf(conn, "RFB 003.%03d\n", minor); err != nil {
		return nil, err
	}

	if minor == 3 {
		var secType uint32
		if err := binary.Read(r, binary.BigEndian, &secType); err != nil {
			return nil, err
		}
		if secType != 1 {
			return nil, rfbRefused(r, secType)
		}
	} else {
		n, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, rfbRefused(r, 0)
		}
		types := make([]byte, n)
		if _, err := io.ReadFull(r, types); err != nil {
			return nil, err
		}
		none := false
		for _, t := range types {
			none = none || t == 1
		}
		if !none {
			return nil, fmt.Errorf("rfb: display needs authentication (security types %v); only None is supported", types)
		}
		if _, err := conn.Write([]byte{1}); err != nil {
			return nil, err
		}
		if minor == 8 {
			var result uint32
			if err := binary.Read(r, binary.BigEndian, &result); err != nil {
				return nil, err
			}
			if result != 0 {
				return nil, rfbRefused(r, 0)
			}
		}
	}

	if _, err := conn.Write([]byte{1}); err != nil { // ClientInit: shared
		return nil, err
	}
	var init struct {
		Width, Height uint16
		PixelFormat   [16]byte
		NameLen       uint32
	}
	if err := binary.Read(r, binary.BigEndian, &init); err != nil {
		return nil, fmt.Errorf("rfb: server init: %w", err)
	}
	if _, err := io.CopyN(io.Discard, r, int64(init.NameLen)); err != nil {
		return nil, err
	}
	w, h := int(init.Width), int(init.Height)
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("rfb: empty framebuffer (display not initialised yet?)")
	}

	// SetPixelFormat: 32 bpp, depth 24, little-endian, true colour,
	// red/green/blue max 255 at shifts 16/8/0.
	msg := []byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}
	// SetEncodings: Raw only.
	msg = append(msg, 2, 0, 0, 1, 0, 0, 0, 0)
	// FramebufferUpdateRequest, non-incremental, whole screen.
	msg = append(msg, 3, 0, 0, 0, 0, 0, byte(w>>8), byte(w), byte(h>>8), byte(h))
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	covered := 0
	for covered < w*h {
		t, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("rfb: read message: %w", err)
		}
		switch t {
		case 0: // FramebufferUpdate
			var hdr struct {
				Pad   byte
				Rects uint16
			}
			if err := binary.Read(r, binary.BigEndian, &hdr); err != nil {
				return nil, err
			}
			for i := 0; i < int(hdr.Rects); i++ {
				var rect struct {
					X, Y, W, H uint16
					Encoding   int32
				}
				if err := binary.Read(r, binary.BigEndian, &rect); err != nil {
					return nil, err
				}
				if rect.Encoding != 0 {
					return nil, fmt.Errorf("rfb: server sent encoding %d, asked for Raw", rect.Encoding)
				}
				row := make([]byte, int(rect.W)*4)
				for y := 0; y < int(rect.H); y++ {
					if _, err := io.ReadFull(r, row); err != nil {
						return nil, err
					}
					py := int(rect.Y) + y
					for x := 0; x < int(rect.W); x++ {
						px := int(rect.X) + x
						if px >= w || py >= h {
							continue
						}
						o := img.PixOffset(px, py)
						img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = row[x*4+2], row[x*4+1], row[x*4], 255
					}
				}
				covered += int(rect.W) * int(rect.H)
			}
		case 1: // SetColourMapEntries
			var hdr struct {
				Pad          byte
				First, Count uint16
			}
			if err := binary.Read(r, binary.BigEndian, &hdr); err != nil {
				return nil, err
			}
			if _, err := io.CopyN(io.Discard, r, int64(hdr.Count)*6); err != nil {
				return nil, err
			}
		case 2: // Bell
		case 3: // ServerCutText
			var hdr struct {
				Pad [3]byte
				Len uint32
			}
			if err := binary.Read(r, binary.BigEndian, &hdr); err != nil {
				return nil, err
			}
			if _, err := io.CopyN(io.Discard, r, int64(hdr.Len)); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("rfb: unexpected server message %d", t)
		}
	}
	return img, nil
}

func rfbRefused(r io.Reader, secType uint32) error {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil || n > 4096 {
		return fmt.Errorf("rfb: server refused the connection (security type %d)", secType)
	}
	reason := make([]byte, n)
	_, _ = io.ReadFull(r, reason)
	return fmt.Errorf("rfb: server refused the connection: %s", reason)
}
