package wav

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

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
		if len(buf) < len(CIDInfo) {
			// mirrors the previous behavior of a short read at the start of
			// the chunk: an EOF class error that callers can ignore.
			return fmt.Errorf("failed to read the INFO subchunk - %w", io.EOF)
		}
		if !bytes.Equal(buf[:len(CIDInfo)], CIDInfo) {
			// not an INFO subchunk (e.g. an adtl LIST, not supported yet)
			// TODO: support adtl subchunks
			ch.Drain()
			return nil
		}
		if d.Metadata == nil {
			d.Metadata = &Metadata{}
		}

		// the rest is a list of string entries. Entries are parsed by walking
		// the buffer with explicit offsets instead of a streaming reader: this
		// keeps the walk linear, never copies the whole buffer per entry and
		// lets a trailing word-alignment pad byte be detected instead of being
		// interpreted as the start of another subchunk.
		const subHeaderSize = 8 // 4 bytes ID + 4 bytes size
		pos := len(CIDInfo)
		for pos < len(buf) {
			// Fewer bytes than a subchunk header can only be the optional
			// padding byte that keeps the chunk word aligned.
			if len(buf)-pos < subHeaderSize {
				break
			}
			var id [4]byte
			copy(id[:], buf[pos:pos+4])
			size := int(binary.LittleEndian.Uint32(buf[pos+4 : pos+8]))
			pos += subHeaderSize
			if size > len(buf)-pos {
				return fmt.Errorf("invalid %s subchunk size %d, only %d bytes left - %w",
					string(id[:]), size, len(buf)-pos, io.ErrUnexpectedEOF)
			}
			scratch := buf[pos : pos+size]
			pos += size
			switch id {
			case markerIARL:
				d.Metadata.Location = nullTermStr(scratch)
			case markerIART:
				d.Metadata.Artist = nullTermStr(scratch)
			case markerISFT:
				d.Metadata.Software = nullTermStr(scratch)
			case markerICRD:
				d.Metadata.CreationDate = nullTermStr(scratch)
			case markerICOP:
				d.Metadata.Copyright = nullTermStr(scratch)
			case markerINAM:
				d.Metadata.Title = nullTermStr(scratch)
			case markerIENG:
				d.Metadata.Engineer = nullTermStr(scratch)
			case markerIGNR:
				d.Metadata.Genre = nullTermStr(scratch)
			case markerIPRD:
				d.Metadata.Product = nullTermStr(scratch)
			case markerISRC:
				d.Metadata.Source = nullTermStr(scratch)
			case markerISBJ:
				d.Metadata.Subject = nullTermStr(scratch)
			case markerICMT:
				d.Metadata.Comments = nullTermStr(scratch)
			case markerITRK, markerITRKBug:
				d.Metadata.TrackNbr = nullTermStr(scratch)
			case markerITCH:
				d.Metadata.Technician = nullTermStr(scratch)
			case markerIKEY:
				d.Metadata.Keywords = nullTermStr(scratch)
			case markerIMED:
				d.Metadata.Medium = nullTermStr(scratch)
			}

			// entries with an odd size are followed by a single word-alignment
			// pad byte; only consume it when it is actually present.
			if size%2 == 1 && pos < len(buf) {
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
		// RIFF subchunks are word aligned: an odd sized value must be
		// followed by a single padding byte so the next header starts on an
		// even boundary. The declared size stays the unpadded value length.
		size := len(val) + 1
		binary.Write(buf, binary.LittleEndian, uint32(size))
		buf.Write(append([]byte(val), 0x00))
		if size%2 == 1 {
			buf.WriteByte(0x00)
		}
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
