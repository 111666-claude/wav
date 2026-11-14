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
		var n int
		if n, err = ch.Read(buf); err != nil {
			return fmt.Errorf("failed to read the LIST chunk - %w", err)
		}
		// The declared chunk size can include a trailing word-alignment byte
		// that isn't present in the underlying data (or be corrupt): only
		// parse the bytes that were actually read.
		buf = buf[:n]
		r := bytes.NewReader(buf)
		// INFO subchunk
		scratch := make([]byte, 4)
		if _, err = r.Read(scratch); err != nil {
			return fmt.Errorf("failed to read the INFO subchunk - %w", err)
		}
		if !bytes.Equal(scratch, CIDInfo[:]) {
			// "expected an INFO subchunk but got %s", string(scratch)
			// TODO: support adtl subchunks
			ch.Drain()
			return nil
		}
		if d.Metadata == nil {
			d.Metadata = &Metadata{}
		}

		// The rest is a list of string entries. The whole chunk is already
		// in memory, so walk it with an index: the work stays linear in the
		// number of entries and the values aren't copied out of the buffer.
		var id [4]byte
		for pos := 4; pos < len(buf); {
			if len(buf)-pos < 8 {
				// Fewer bytes than a subchunk header remain. Writers may
				// append a trailing word-alignment byte to the chunk; don't
				// treat those leftover bytes as a new subchunk (this used
				// to surface as a spurious unexpected EOF).
				break
			}
			copy(id[:], buf[pos:pos+4])
			size := binary.LittleEndian.Uint32(buf[pos+4 : pos+8])
			pos += 8
			avail := len(buf) - pos
			dataLen := int(size)
			if size > uint32(avail) {
				// Corrupt declared length: clamp to the data that's
				// actually there, preserving the historical lenient
				// behavior instead of failing.
				dataLen = avail
			}
			data := buf[pos : pos+dataLen]
			pos += dataLen
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
			if size%2 == 1 && pos < len(buf) && buf[pos] == 0x00 {
				// Subchunks may be word-aligned with a NUL pad byte, but
				// some writers pack them back to back instead. Only skip
				// the byte when it looks like padding: a packed subchunk
				// ID never starts with a NUL byte.
				pos++
			}
		}
	}
	ch.Drain()
	return nil
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
