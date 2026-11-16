package wav

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// validateMetadataForEncoding runs the structural checks that do not depend
// on the audio length: unknown cue references and conflicting duplicate
// entries. Segment bounds are verified by the decoder once the PCM size is
// known (Metadata.Validate).
func validateMetadataForEncoding(md *Metadata) error {
	return md.Validate(0)
}

// encodeCueChunk serializes the cue chunk. It returns nil when there is no
// cue point to write.
//
//	4 bytes: number of cue points
//	per cue point (24 bytes):
//	  ID, position, data chunk ID, chunk start, block start, sample offset
func encodeCueChunk(e *Encoder) []byte {
	if e == nil || e.Metadata == nil || len(e.Metadata.CuePoints) == 0 {
		return nil
	}
	buf := bytes.NewBuffer(nil)
	binary.Write(buf, binary.LittleEndian, uint32(len(e.Metadata.CuePoints)))
	for _, c := range e.Metadata.CuePoints {
		if c == nil {
			continue
		}
		buf.Write(c.ID[:])
		binary.Write(buf, binary.LittleEndian, c.Position)
		buf.Write(c.DataChunkID[:])
		binary.Write(buf, binary.LittleEndian, c.ChunkStart)
		binary.Write(buf, binary.LittleEndian, c.BlockStart)
		binary.Write(buf, binary.LittleEndian, c.SampleOffset)
	}
	return buf.Bytes()
}

// encodeAdtlChunk serializes the payload of a LIST "adtl" chunk (the "adtl"
// type marker plus the labl/note/ltxt sub chunks). It returns nil when there
// is nothing to write. Sub chunk padding is handled by writeChunk for the
// wrapping LIST and added locally between sub chunks.
func encodeAdtlChunk(e *Encoder) ([]byte, error) {
	if e == nil || e.Metadata == nil {
		return nil, nil
	}
	md := e.Metadata
	if len(md.CueLabels) == 0 && len(md.CueNotes) == 0 && len(md.CueSegments) == 0 {
		return nil, nil
	}
	out := bytes.NewBuffer(nil)
	out.Write(markerAdtl[:])

	// writeTextChunk serializes an labl/note sub chunk:
	// 4 byte ID, 4 byte size, cue ID, null terminated UTF-8 text, padding.
	writeTextChunk := func(marker [4]byte, cueID [4]byte, text string) error {
		if err := checkText(text, true); err != nil {
			return fmt.Errorf("%s %s: %w", marker, fourCC(cueID), err)
		}
		payload := bytes.NewBuffer(nil)
		payload.Write(cueID[:])
		payload.WriteString(text)
		payload.WriteByte(0x00) // null terminator
		out.Write(marker[:])
		binary.Write(out, binary.LittleEndian, uint32(payload.Len()))
		out.Write(payload.Bytes())
		if payload.Len()%2 == 1 {
			out.WriteByte(0x00) // word alignment padding
		}
		return nil
	}

	for _, l := range md.CueLabels {
		if l == nil {
			continue
		}
		if err := writeTextChunk(markerLabl, l.CueID, l.Text); err != nil {
			return nil, err
		}
	}
	for _, n := range md.CueNotes {
		if n == nil {
			continue
		}
		if err := writeTextChunk(markerNote, n.CueID, n.Text); err != nil {
			return nil, err
		}
	}
	for _, s := range md.CueSegments {
		if s == nil {
			continue
		}
		if err := checkText(s.Text, false); err != nil {
			return nil, fmt.Errorf("ltxt %s: %w", fourCC(s.CueID), err)
		}
		payload := bytes.NewBuffer(nil)
		payload.Write(s.CueID[:])
		binary.Write(payload, binary.LittleEndian, s.SampleLength)
		payload.Write(s.Purpose[:])
		binary.Write(payload, binary.LittleEndian, s.Country)
		binary.Write(payload, binary.LittleEndian, s.Language)
		binary.Write(payload, binary.LittleEndian, s.Dialect)
		binary.Write(payload, binary.LittleEndian, s.CodePage)
		if s.Text != "" {
			payload.WriteString(s.Text)
			payload.WriteByte(0x00)
		}
		out.Write(markerLtxt[:])
		binary.Write(out, binary.LittleEndian, uint32(payload.Len()))
		out.Write(payload.Bytes())
		if payload.Len()%2 == 1 {
			out.WriteByte(0x00) // word alignment padding
		}
	}

	if uint64(out.Len()) > maxChunkSize {
		return nil, fmt.Errorf("%w: adtl LIST payload is %d bytes", ErrTextTooLong, out.Len())
	}
	return out.Bytes(), nil
}
