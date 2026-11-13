package wav

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/go-audio/riff"
)

var (
	// See http://bwfmetaedit.sourceforge.net/listinfo.html
	markerIART    = [4]byte{'I', 'A', 'R', 'T'}
	markerISFT    = [4]byte{'I', 'S', 'F', 'T'}
	markerICRD    = [4]byte{'I', 'C', 'R', 'D'}
	markerICOP    = [4]byte{'I', 'C', 'O', 'P'}
	markerIARL    = [4]byte{'I', 'A', 'R', 'L'}
	markerINAM    = [4]byte{'I', 'N', 'A', 'M'}
	markerIENG    = [4]byte{'I', 'E', 'N', 'G'}
	markerIGNR    = [4]byte{'I', 'G', 'N', 'R'}
	markerIPRD    = [4]byte{'I', 'P', 'R', 'D'}
	markerISRC    = [4]byte{'I', 'S', 'R', 'C'}
	markerISBJ    = [4]byte{'I', 'S', 'B', 'J'}
	markerICMT    = [4]byte{'I', 'C', 'M', 'T'}
	markerITRK    = [4]byte{'I', 'T', 'R', 'K'}
	markerITRKBug = [4]byte{'i', 't', 'r', 'k'}
	markerITCH    = [4]byte{'I', 'T', 'C', 'H'}
	markerIKEY    = [4]byte{'I', 'K', 'E', 'Y'}
	markerIMED    = [4]byte{'I', 'M', 'E', 'D'}
)

// DecodeListChunk decodes a LIST chunk
func DecodeListChunk(d *Decoder, ch *riff.Chunk) error {
	if ch == nil {
		return fmt.Errorf("can't decode a nil chunk")
	}
	if d == nil {
		return fmt.Errorf("nil decoder")
	}
	if ch.ID == CIDList {
		// read the entire chunk in memory
		buf := make([]byte, ch.Size)
		var err error
		if _, err = ch.Read(buf); err != nil {
			return fmt.Errorf("failed to read the LIST chunk - %w", err)
		}
		// INFO subchunk
		// Keep the historical error path (and its wrapping) when the chunk is
		// too short to hold an INFO marker; callers such as ReadMetadata
		// rely on this error being swallowable as EOF.
		r := bytes.NewReader(buf)
		scratch := make([]byte, len(CIDInfo))
		if _, err = r.Read(scratch); err != nil {
			return fmt.Errorf("failed to read the INFO subchunk - %w", err)
		}
		if !bytes.Equal(scratch, CIDInfo) {
			// "expected an INFO subchunk but got %s", string(buf[:4])
			// TODO: support adtl subchunks
			ch.Drain()
			return nil
		}
		if d.Metadata == nil {
			d.Metadata = &Metadata{}
		}

		// The rest is a list of string entries. Entries are walked with an
		// offset instead of a reader so the entry values can be sliced
		// directly out of buf: no per-entry scratch buffer is allocated and
		// no bytes are copied, which keeps parsing linear regardless of the
		// number of subchunks.
		//
		// Each entry is laid out as: 4 byte ID, 4 byte little-endian size,
		// size data bytes, plus a single zero padding byte when size is odd
		// (RIFF words are 2 byte aligned). The outer chunk size reported by
		// the RIFF parser already includes that trailing padding, so it must
		// be consumed here rather than mistaken for the start of another
		// subchunk.
		const headerSize = 8
		off := len(CIDInfo)
		for off+headerSize <= len(buf) {
			var id [4]byte
			copy(id[:], buf[off:off+4])
			size := int(binary.LittleEndian.Uint32(buf[off+4 : off+8]))
			dataStart := off + headerSize
			dataEnd := dataStart + size
			if dataEnd > len(buf) {
				// Declared size runs past the chunk. Preserve the historical
				// behavior of stopping without raising a new fatal error.
				break
			}
			data := buf[dataStart:dataEnd]

			switch id {
			case markerIARL:
				d.Metadata.Location = nullTermStr(data)
			case markerIART:
				d.Metadata.Artist = nullTermStr(data)
			case markerISFT:
				d.Metadata.Software = nullTermStr(data)
			case markerICRD:
				d.Metadata.CreationDate = nullTermStr(data)
			case markerICOP:
				d.Metadata.Copyright = nullTermStr(data)
			case markerINAM:
				d.Metadata.Title = nullTermStr(data)
			case markerIENG:
				d.Metadata.Engineer = nullTermStr(data)
			case markerIGNR:
				d.Metadata.Genre = nullTermStr(data)
			case markerIPRD:
				d.Metadata.Product = nullTermStr(data)
			case markerISRC:
				d.Metadata.Source = nullTermStr(data)
			case markerISBJ:
				d.Metadata.Subject = nullTermStr(data)
			case markerICMT:
				d.Metadata.Comments = nullTermStr(data)
			case markerITRK, markerITRKBug:
				d.Metadata.TrackNbr = nullTermStr(data)
			case markerITCH:
				d.Metadata.Technician = nullTermStr(data)
			case markerIKEY:
				d.Metadata.Keywords = nullTermStr(data)
			case markerIMED:
				d.Metadata.Medium = nullTermStr(data)
			}
			// Unknown IDs are intentionally skipped.

			off = dataEnd
			if size%2 == 1 {
				// RIFF subchunks are word aligned, so an odd sized entry is
				// normally followed by a single padding byte. Some encoders
				// omit that byte between entries though, so only treat the
				// byte as padding when it is unambiguous: it is either the
				// last byte of the chunk, or a valid INFO-style subchunk ID
				// starts right after it.
				if off < len(buf) && (off+1 == len(buf) || looksLikeInfoID(buf[off+1:])) {
					off++
				}
			}
		}
	}
	ch.Drain()
	return nil
}

// looksLikeInfoID reports whether the 4 bytes available at b start with the
// shape of an INFO subchunk ID: uppercase ASCII letters, which lets the
// parser tell a word-alignment padding byte apart from the first byte of
// the next subchunk header.
func looksLikeInfoID(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	for _, c := range b[:4] {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

func encodeInfoChunk(e *Encoder) []byte {
	if e == nil || e.Metadata == nil {
		return nil
	}
	buf := bytes.NewBuffer(nil)

	writeSection := func(id [4]byte, val string) {
		buf.Write(id[:])
		binary.Write(buf, binary.LittleEndian, uint32(len(val)+1))
		buf.Write(append([]byte(val), 0x00))
	}
	if e.Metadata.Artist != "" {
		writeSection(markerIART, e.Metadata.Artist)
	}
	if e.Metadata.Comments != "" {
		writeSection(markerICMT, e.Metadata.Comments)
	}
	if e.Metadata.Copyright != "" {
		writeSection(markerICOP, e.Metadata.Copyright)
	}
	if e.Metadata.CreationDate != "" {
		writeSection(markerICRD, e.Metadata.CreationDate)
	}
	if e.Metadata.Engineer != "" {
		writeSection(markerIENG, e.Metadata.Engineer)
	}
	if e.Metadata.Technician != "" {
		writeSection(markerITCH, e.Metadata.Technician)
	}
	if e.Metadata.Genre != "" {
		writeSection(markerIGNR, e.Metadata.Genre)
	}
	if e.Metadata.Keywords != "" {
		writeSection(markerIKEY, e.Metadata.Keywords)
	}
	if e.Metadata.Medium != "" {
		writeSection(markerIMED, e.Metadata.Medium)
	}
	if e.Metadata.Title != "" {
		writeSection(markerINAM, e.Metadata.Title)
	}
	if e.Metadata.Product != "" {
		writeSection(markerIPRD, e.Metadata.Product)
	}
	if e.Metadata.Subject != "" {
		writeSection(markerISBJ, e.Metadata.Subject)
	}
	if e.Metadata.Software != "" {
		writeSection(markerISFT, e.Metadata.Software)
	}
	if e.Metadata.Source != "" {
		writeSection(markerISRC, e.Metadata.Source)
	}
	if e.Metadata.Location != "" {
		writeSection(markerIARL, e.Metadata.Location)
	}
	if e.Metadata.TrackNbr != "" {
		writeSection(markerITRK, e.Metadata.TrackNbr)
	}

	return append(CIDInfo, buf.Bytes()...)
}
