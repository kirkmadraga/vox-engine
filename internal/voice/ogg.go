package voice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// OggReader extracts raw Opus packets from an Ogg Opus stream (RFC 7845),
// without decoding them. It supports a single logical stream (what yt-dlp and
// ffmpeg produce), verifies page checksums, and joins packets that span pages.
type OggReader struct {
	r        io.Reader
	serial   uint32
	started  bool
	lacing   []byte // unread lacing values of the current page
	data     []byte // unread payload of the current page
	pending  []byte // packet continued from a previous page
	Channels int    // from the OpusHead header
}

// ErrNotOpus means the stream is Ogg but not Ogg Opus, or its headers are invalid.
var ErrNotOpus = errors.New("not an Ogg Opus stream")

// NewOggReader reads and validates the OpusHead and OpusTags header packets.
func NewOggReader(r io.Reader) (*OggReader, error) {
	o := &OggReader{r: r}
	head, err := o.nextRaw()
	if err != nil {
		return nil, fmt.Errorf("read OpusHead: %w", err)
	}
	// OpusHead: magic(8) version(1) channels(1) pre-skip(2) rate(4) gain(2) mapping(1)
	if len(head) < 19 || !bytes.HasPrefix(head, []byte("OpusHead")) || head[8]>>4 != 0 {
		return nil, ErrNotOpus
	}
	o.Channels = int(head[9])
	tags, err := o.nextRaw()
	if err != nil {
		return nil, fmt.Errorf("read OpusTags: %w", err)
	}
	if !bytes.HasPrefix(tags, []byte("OpusTags")) {
		return nil, ErrNotOpus
	}
	return o, nil
}

// NextPacket returns the next Opus packet, or io.EOF at the end of the stream.
// The returned slice is not reused by later calls.
func (o *OggReader) NextPacket() ([]byte, error) {
	for {
		p, err := o.nextRaw()
		if err != nil || len(p) > 0 {
			return p, err
		}
		// Skip empty packets; Opus never produces them.
	}
}

// nextRaw returns the next packet, reading pages as needed.
func (o *OggReader) nextRaw() ([]byte, error) {
	for {
		for len(o.lacing) > 0 {
			n := int(o.lacing[0])
			o.lacing = o.lacing[1:]
			o.pending = append(o.pending, o.data[:n]...)
			o.data = o.data[n:]
			if n < 255 { // a lacing value below 255 ends the packet
				p := o.pending
				o.pending = nil
				if p == nil {
					p = []byte{}
				}
				return p, nil
			}
		}
		if err := o.readPage(); err != nil {
			if errors.Is(err, io.EOF) && len(o.pending) > 0 {
				return nil, io.ErrUnexpectedEOF // stream ended mid-packet
			}
			return nil, err
		}
	}
}

// readPage reads one page into o.lacing and o.data. It returns io.EOF only
// at a clean page boundary.
func (o *OggReader) readPage() error {
	var hdr [27]byte
	if _, err := io.ReadFull(o.r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("ogg page header: %w", noEOF(err))
	}
	if string(hdr[:4]) != "OggS" || hdr[4] != 0 {
		return errors.New("ogg: bad page header")
	}
	serial := binary.LittleEndian.Uint32(hdr[14:18])
	if !o.started {
		o.serial, o.started = serial, true
	} else if serial != o.serial {
		return errors.New("ogg: multiple logical streams are not supported")
	}

	lacing := make([]byte, hdr[26])
	if _, err := io.ReadFull(o.r, lacing); err != nil {
		return fmt.Errorf("ogg segment table: %w", noEOF(err))
	}
	size := 0
	for _, l := range lacing {
		size += int(l)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(o.r, data); err != nil {
		return fmt.Errorf("ogg page data: %w", noEOF(err))
	}

	want := binary.LittleEndian.Uint32(hdr[22:26])
	binary.LittleEndian.PutUint32(hdr[22:26], 0) // the CRC is computed with its own field zeroed
	crc := oggCRC(0, hdr[:])
	crc = oggCRC(crc, lacing)
	crc = oggCRC(crc, data)
	if crc != want {
		return errors.New("ogg: page checksum mismatch (corrupt file)")
	}

	o.lacing, o.data = lacing, data
	return nil
}

// noEOF turns a mid-structure EOF into ErrUnexpectedEOF.
func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// oggCRCTable is CRC-32 with polynomial 0x04c11db7, unreflected, as Ogg specifies.
var oggCRCTable = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

func oggCRC(crc uint32, b []byte) uint32 {
	for _, c := range b {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^c]
	}
	return crc
}
