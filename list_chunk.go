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
	if ch.ID != CIDList {
		ch.Drain()
		return nil
	}

	// read the entire chunk in memory
	buf := make([]byte, ch.Size)
	if _, err := io.ReadFull(ch, buf); err != nil {
		return fmt.Errorf("failed to read the LIST chunk - %w", err)
	}
	// INFO subchunk
	if len(buf) < len(CIDInfo) {
		return fmt.Errorf("failed to read the INFO subchunk - %w", io.ErrUnexpectedEOF)
	}
	if !bytes.Equal(buf[:len(CIDInfo)], CIDInfo) {
		// "expected an INFO subchunk but got %s", string(buf[:4])
		// TODO: support adtl subchunks
		ch.Drain()
		return nil
	}
	if d.Metadata == nil {
		d.Metadata = &Metadata{}
	}

	// The rest is a list of string entries. Each entry is a 4 byte ID, a
	// 4 byte little endian size and `size` bytes of data. RIFF requires every
	// chunk (and therefore every INFO entry) to be word aligned: an entry with
	// an odd size is followed by a single padding byte which is not counted in
	// the entry size but is part of the enclosing LIST chunk.
	r := bytes.NewReader(buf[len(CIDInfo):])
	remaining := len(buf) - len(CIDInfo)
	for remaining > 0 {
		if remaining == 1 {
			// A single byte can only be the word alignment byte of the LIST
			// chunk itself (when the RIFF parser rounded an odd chunk size up).
			// Consume it and stop rather than trying to read a sub header.
			if _, err := r.ReadByte(); err != nil {
				return fmt.Errorf("failed to read the LIST alignment byte - %w", err)
			}
			remaining = 0
			break
		}
		if remaining < 8 {
			return fmt.Errorf("truncated INFO subchunk header: %d byte(s) remaining", remaining)
		}

		var (
			id   [4]byte
			size uint32
		)
		if err := binary.Read(r, binary.BigEndian, &id); err != nil {
			return fmt.Errorf("read sub header: %w", err)
		}
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
			return fmt.Errorf("read sub header %s size: %w", id, err)
		}
		remaining -= 8
		if int64(size) > int64(remaining) {
			return fmt.Errorf("INFO subchunk %s declares %d byte(s) but only %d remain", id, size, remaining)
		}

		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return fmt.Errorf("read sub header %s data: %w", id, err)
		}
		remaining -= int(size)

		if size%2 == 1 {
			// Odd sized entries consume exactly one alignment byte.
			if remaining < 1 {
				return fmt.Errorf("INFO subchunk %s of size %d is missing its alignment byte", id, size)
			}
			if _, err := r.ReadByte(); err != nil {
				return fmt.Errorf("read sub header %s alignment byte: %w", id, err)
			}
			remaining--
		}

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
		// The declared size includes the null terminator.
		size := uint32(len(val) + 1)
		payload := make([]byte, 0, int(size)+1)
		payload = append(payload, val...)
		payload = append(payload, 0x00)

		buf.Write(id[:])
		binary.Write(buf, binary.LittleEndian, size)
		buf.Write(payload)
		// RIFF chunks are word aligned: an odd sized entry is followed by a
		// single padding byte that isn't counted in its declared size.
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
