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
		// The chunk size declares the data length without the trailing word
		// alignment padding byte. Read exactly ch.Size bytes so the chunk
		// boundary stays at its declared length regardless of padding.
		buf := make([]byte, ch.Size)
		if _, err := io.ReadFull(ch, buf); err != nil {
			return fmt.Errorf("failed to read the LIST chunk - %v", err)
		}
		r := bytes.NewReader(buf)
		// INFO subchunk
		scratch := make([]byte, 4)
		if _, err := io.ReadFull(r, scratch); err != nil {
			return fmt.Errorf("failed to read the INFO subchunk - %v", err)
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

		// the rest is a list of string entries
		var (
			id   [4]byte
			size uint32
		)
		readSubHeader := func() error {
			if _, err := io.ReadFull(r, id[:]); err != nil {
				return err
			}
			var hdr [4]byte
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				return err
			}
			size = binary.LittleEndian.Uint32(hdr[:])
			return nil
		}

		for {
			// stop exactly at the declared chunk boundary; 1..7 trailing bytes
			// can't form another entry and are simply ignored
			if r.Len() < 8 {
				break
			}
			if err := readSubHeader(); err != nil {
				return fmt.Errorf("failed to read a LIST/INFO entry header - %v", err)
			}
			if uint64(size) > uint64(r.Len()) {
				return fmt.Errorf("truncated LIST/INFO entry %s: declared size %d but only %d bytes remain", id, size, r.Len())
			}
			scratch = make([]byte, size)
			if _, err := io.ReadFull(r, scratch); err != nil {
				return fmt.Errorf("failed to read the %s entry data - %v", id, err)
			}
			// RIFF entries are word aligned: an odd sized entry is followed by
			// a single zero padding byte which is not part of the declared size.
			if size%2 == 1 {
				if r.Len() < 1 {
					return fmt.Errorf("truncated LIST/INFO entry %s: missing word alignment padding byte", id)
				}
				if _, err := r.Seek(1, io.SeekCurrent); err != nil {
					return err
				}
			}
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
		// the declared entry size includes the null terminator but not the
		// word alignment padding
		entrySize := uint32(len(val) + 1)
		binary.Write(buf, binary.LittleEndian, entrySize)
		buf.Write(append([]byte(val), 0x00))
		// RIFF entries must be word aligned: pad an odd sized entry with a
		// single zero byte that is not part of the declared size
		if entrySize%2 == 1 {
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
