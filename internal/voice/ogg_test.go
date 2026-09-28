package voice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"
)

// oggWriter builds Ogg streams for tests: each page is given as a list of
// packets, and a trailing continued packet is expressed with cont=true.
type oggWriter struct {
	buf    bytes.Buffer
	serial uint32
	seq    uint32
}

// page writes one page containing the given segments' lacing. Each element of
// packets is laced fully (terminated) unless it is the last and cont is set.
func (w *oggWriter) page(packets [][]byte, cont bool) {
	var lacing []byte
	var data []byte
	for i, p := range packets {
		data = append(data, p...)
		n := len(p)
		for n >= 255 {
			lacing = append(lacing, 255)
			n -= 255
		}
		last := i == len(packets)-1
		if !(last && cont) {
			lacing = append(lacing, byte(n))
		} else if n > 0 {
			panic("continued packet must be a multiple of 255 bytes in tests")
		}
	}
	hdr := make([]byte, 27)
	copy(hdr, "OggS")
	binary.LittleEndian.PutUint32(hdr[14:], w.serial)
	binary.LittleEndian.PutUint32(hdr[18:], w.seq)
	hdr[26] = byte(len(lacing))
	w.seq++
	crc := oggCRC(oggCRC(oggCRC(0, hdr), lacing), data)
	binary.LittleEndian.PutUint32(hdr[22:], crc)
	w.buf.Write(hdr)
	w.buf.Write(lacing)
	w.buf.Write(data)
}

func opusHead(channels byte) []byte {
	h := []byte("OpusHead")
	return append(h, 1, channels, 0x38, 0x01, 0x80, 0xBB, 0, 0, 0, 0, 0)
}

// newStream returns a writer with valid OpusHead and OpusTags pages.
func newStream() *oggWriter {
	w := &oggWriter{serial: 42}
	w.page([][]byte{opusHead(2)}, false)
	w.page([][]byte{[]byte("OpusTagsvendor")}, false)
	return w
}

// frame20 returns a 20 ms CELT packet (config 31, code 0) of the given size.
func frame20(size int, fill byte) []byte {
	p := bytes.Repeat([]byte{fill}, size)
	p[0] = 31 << 3
	return p
}

func readAll(t *testing.T, data []byte) ([][]byte, error) {
	t.Helper()
	r, err := NewOggReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for {
		p, err := r.NextPacket()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, p)
	}
}

func TestOggMultiplePacketsPerPage(t *testing.T) {
	w := newStream()
	a, b, c := frame20(10, 1), frame20(300, 2), frame20(255, 3) // 300 and 255 need multi-segment lacing
	w.page([][]byte{a, b, c}, false)
	got, err := readAll(t, w.buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) || !bytes.Equal(got[2], c) {
		t.Errorf("packets not extracted intact: got %d packets", len(got))
	}
}

func TestOggPacketSpanningPages(t *testing.T) {
	w := newStream()
	big := frame20(255*3+17, 7)
	w.page([][]byte{frame20(5, 1), big[:255*2]}, true) // first 510 bytes, continued
	w.page([][]byte{big[255*2:]}, false)               // remaining bytes
	got, err := readAll(t, w.buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !bytes.Equal(got[1], big) {
		t.Fatalf("continued packet not reassembled: %d packets", len(got))
	}
}

func TestOggReturnedSlicesAreNotReused(t *testing.T) {
	w := newStream()
	w.page([][]byte{frame20(4, 1), frame20(4, 2)}, false)
	r, _ := NewOggReader(bytes.NewReader(w.buf.Bytes()))
	first, _ := r.NextPacket()
	keep := bytes.Clone(first)
	r.NextPacket()
	if !bytes.Equal(first, keep) {
		t.Error("first packet changed after reading the second")
	}
}

func TestOggErrors(t *testing.T) {
	valid := func() *oggWriter { w := newStream(); w.page([][]byte{frame20(8, 1)}, false); return w }

	t.Run("not ogg", func(t *testing.T) {
		if _, err := readAll(t, []byte("RIFF....WAVEfmt ")); err == nil {
			t.Error("expected error")
		}
	})
	t.Run("not opus", func(t *testing.T) {
		w := &oggWriter{}
		w.page([][]byte{[]byte("\x01vorbis-header-bytes-here")}, false)
		if _, err := readAll(t, w.buf.Bytes()); !errors.Is(err, ErrNotOpus) {
			t.Errorf("err = %v, want ErrNotOpus", err)
		}
	})
	t.Run("missing tags", func(t *testing.T) {
		w := &oggWriter{}
		w.page([][]byte{opusHead(2), frame20(8, 1)}, false)
		if _, err := readAll(t, w.buf.Bytes()); !errors.Is(err, ErrNotOpus) {
			t.Errorf("err = %v, want ErrNotOpus", err)
		}
	})
	t.Run("bad checksum", func(t *testing.T) {
		data := valid().buf.Bytes()
		data[len(data)-1] ^= 0xFF // flip a payload byte of the last page
		if _, err := readAll(t, data); err == nil {
			t.Error("expected checksum error")
		}
	})
	t.Run("truncated page", func(t *testing.T) {
		data := valid().buf.Bytes()
		if _, err := readAll(t, data[:len(data)-3]); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("err = %v, want ErrUnexpectedEOF", err)
		}
	})
	t.Run("ends mid-packet", func(t *testing.T) {
		w := newStream()
		w.page([][]byte{bytes.Repeat([]byte{1}, 255)}, true)
		if _, err := readAll(t, w.buf.Bytes()); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("err = %v, want ErrUnexpectedEOF", err)
		}
	})
	t.Run("second logical stream", func(t *testing.T) {
		w := valid()
		w.serial = 99
		w.page([][]byte{frame20(8, 1)}, false)
		if _, err := readAll(t, w.buf.Bytes()); err == nil {
			t.Error("expected multiplexed stream error")
		}
	})
}

// TestOggRealFixture reads a 1 s tone produced by ffmpeg with -frame_duration 20.
func TestOggRealFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/tone-1s.opus")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewOggReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if r.Channels != 2 {
		t.Errorf("channels = %d, want 2", r.Channels)
	}
	n := 0
	for {
		p, err := r.NextPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		d, err := PacketDuration(p)
		if err != nil || d != FrameDuration {
			t.Fatalf("packet %d: duration %v, err %v", n, d, err)
		}
		n++
	}
	// 1 s of audio plus encoder pre-skip: 50 or 51 packets of 20 ms.
	if n < 50 || n > 51 {
		t.Errorf("got %d packets, want 50-51", n)
	}
}
