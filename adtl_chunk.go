package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/go-audio/riff"
)

// The Associated Data List ("adtl") is a LIST chunk holding text and
// segment annotations referencing cue points by ID. It is documented in the
// Microsoft New Multimedia Data Types and Data Techniques (RTC 151) /
// Multimedia Programmer's Reference:
//
//	LIST "adtl"
//	  labl <cue ID, null terminated text>   // label of a cue point
//	  note <cue ID, null terminated text>   // free form comment
//	  ltxt <cue ID, sample length, purpose, country, language, dialect, code page, optional text>
//
// Every sub chunk and the wrapping LIST chunk are padded with a zero byte when
// their data occupies an odd number of bytes.

var (
	markerAdtl = [4]byte{'a', 'd', 't', 'l'}
	markerLabl = [4]byte{'l', 'a', 'b', 'l'}
	markerNote = [4]byte{'n', 'o', 't', 'e'}
	markerLtxt = [4]byte{'l', 't', 'x', 't'}
)

// Sentinel errors returned while decoding or validating the cue point
// annotations. They are wrapped with contextual information, callers should
// use errors.Is to match them.
var (
	// ErrUnknownCueID is returned when an adtl entry references a cue ID that
	// is not declared in the cue chunk.
	ErrUnknownCueID = errors.New("adtl entry references an unknown cue ID")
	// ErrDuplicateAdtlEntry is returned when several labl/note/ltxt entries
	// target the same cue ID (a cue point can only carry one of each).
	ErrDuplicateAdtlEntry = errors.New("duplicate adtl entry for cue ID")
	// ErrTruncatedChunk is returned when a sub chunk claims more data than the
	// enclosing LIST chunk actually contains, or when a fixed size field set
	// cannot be fully read.
	ErrTruncatedChunk = errors.New("truncated chunk")
	// ErrTextTooLong is returned when a text string cannot be represented in a
	// WAV sub chunk (the sub chunk size is a 32 bit value).
	ErrTextTooLong = errors.New("text too long for a WAV sub chunk")
	// ErrSegmentOutOfRange is returned when a labeled segment (ltxt) reaches
	// past the end of the PCM data.
	ErrSegmentOutOfRange = errors.New("labeled segment extends past the audio data")
	// ErrInvalidUTF8 is returned when a text string is not valid UTF-8.
	ErrInvalidUTF8 = errors.New("text is not valid UTF-8")
)

// maxChunkSize is the largest amount of data a single RIFF sub chunk can
// carry: its size field is an unsigned 32 bit integer.
const maxChunkSize = 0xFFFFFFFF

// CueLabel stores the content of an "labl" sub chunk: the name assigned to a
// cue point identified by CueID.
type CueLabel struct {
	// CueID is the ID of the cue point the label applies to.
	CueID [4]byte
	// Text is the label of the cue point.
	Text string
}

// CueNote stores the content of a "note" sub chunk: a free form comment
// attached to the cue point identified by CueID.
type CueNote struct {
	// CueID is the ID of the cue point the note applies to.
	CueID [4]byte
	// Text is the comment.
	Text string
}

// CueSegment stores the content of an "ltxt" sub chunk: a labeled region of
// the audio starting at the matching cue point and spanning SampleLength
// sample frames.
type CueSegment struct {
	// CueID is the ID of the cue point marking the start of the segment.
	CueID [4]byte
	// SampleLength is the number of sample frames covered by the segment,
	// starting at the referenced cue point.
	SampleLength uint32
	// Purpose describes the purpose of the text. Standard values are
	// "rgn " (region), "scrp" (script) and "capt" (caption).
	Purpose [4]byte
	// Country is the country code of the following text (0 means none).
	Country uint16
	// Language is the language code of the following text (0 means none).
	Language uint16
	// Dialect is the dialect code of the following text (0 means none).
	Dialect uint16
	// CodePage is the code page used to encode the following text (0 means
	// none; modern files use UTF-8 and typically set it to 65001).
	CodePage uint16
	// Text is the optional description of the segment.
	Text string
}

// ltxtFixedFields is the fixed, binary part of an ltxt sub chunk:
// 4 (cue ID) + 4 (sample length) + 4 (purpose) + 2*4 (country/language/dialect/codepage).
const ltxtFixedFields = 20

// DecodeListChunk decodes a LIST chunk. Both the INFO (document tags) and the
// adtl (cue point annotations) list types are supported.
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
	if _, err := ch.Read(buf); err != nil {
		return fmt.Errorf("failed to read the LIST chunk - %w", err)
	}
	r := bytes.NewReader(buf)
	header := make([]byte, 4)
	if _, err := r.Read(header); err != nil {
		return fmt.Errorf("failed to read the LIST type - %w", err)
	}
	switch {
	case bytes.Equal(header, CIDInfo[:]):
		if err := decodeInfoList(d, r, ch.Size); err != nil {
			return err
		}
	case bytes.Equal(header, markerAdtl[:]):
		if err := decodeAdtlList(d, r); err != nil {
			return err
		}
	default:
		// unsupported list type, the full chunk was already read so nothing
		// more has to be drained.
	}
	ch.Drain()
	return nil
}

// decodeInfoList decodes the string entries of a LIST "INFO" chunk. The
// reader is positioned right after the "INFO" marker.
func decodeInfoList(d *Decoder, r *bytes.Reader, chunkSize int) error {
	if d.Metadata == nil {
		d.Metadata = &Metadata{}
	}
	scratch := make([]byte, 4)
	// the rest is a list of string entries
	var (
		id   [4]byte
		size uint32
	)
	readSubHeader := func() error {
		if err := binary.Read(r, binary.BigEndian, &id); err != nil {
			return err
		}
		return binary.Read(r, binary.LittleEndian, &size)
	}

	// This checks and stops early if just a word alignment byte remains to avoid
	// an io.UnexpectedEOF error from readSubHeader.
	// TODO(steve): Remove the checks from the for statement if ch.Size is changed
	// to not include the padding byte.
	for rem := chunkSize - 4; rem > 1; rem -= int(size) + 8 {
		if err := readSubHeader(); err != nil {
			if errors.Is(err, io.EOF) {
				// All done.
				break
			}
			return fmt.Errorf("read sub header: %w", err)
		}

		if cap(scratch) >= int(size) {
			if len(scratch) != int(size) {
				// Resize scratch.
				scratch = scratch[:size]
			}
		} else {
			// Expand scratch capacity.
			scratch = append(make([]byte, int(size)-cap(scratch)), scratch[:cap(scratch)]...)
		}

		if _, err := r.Read(scratch); err != nil {
			return fmt.Errorf("read sub header %s data %v: %w", id, scratch, err)
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
	return nil
}

// decodeAdtlList decodes the labl/note/ltxt entries of a LIST "adtl" chunk.
// The reader is positioned right after the "adtl" marker.
func decodeAdtlList(d *Decoder, r *bytes.Reader) error {
	if d.Metadata == nil {
		d.Metadata = &Metadata{}
	}
	md := d.Metadata
	// bytes remaining inside the LIST chunk, "adtl" (4 bytes) already consumed
	remaining := r.Len()

	seenLabel := map[[4]byte]bool{}
	seenNote := map[[4]byte]bool{}
	seenSegment := map[[4]byte]bool{}

	for remaining > 0 {
		// A single trailing byte is the zero padding of the LIST chunk itself.
		if remaining == 1 {
			var pad byte
			if err := binary.Read(r, binary.LittleEndian, &pad); err != nil {
				return fmt.Errorf("failed to read the LIST padding byte - %w", err)
			}
			remaining = 0
			break
		}
		if remaining < 8 {
			return fmt.Errorf("%w: %d stray byte(s) at the end of the adtl LIST chunk", ErrTruncatedChunk, remaining)
		}
		var (
			subID   [4]byte
			subSize uint32
		)
		if err := binary.Read(r, binary.BigEndian, &subID); err != nil {
			return fmt.Errorf("failed to read an adtl sub chunk ID - %w", err)
		}
		if err := binary.Read(r, binary.LittleEndian, &subSize); err != nil {
			return fmt.Errorf("failed to read an adtl sub chunk size - %w", err)
		}
		remaining -= 8
		if int64(subSize) > int64(remaining) {
			return fmt.Errorf("%w: %s sub chunk claims %d bytes but only %d are available",
				ErrTruncatedChunk, subID, subSize, remaining)
		}
		payload := make([]byte, subSize)
		if _, err := r.Read(payload); err != nil {
			return fmt.Errorf("failed to read the %s sub chunk data - %w", subID, err)
		}
		remaining -= int(subSize)
		// consume the word alignment padding of the sub chunk
		if subSize%2 == 1 {
			if remaining < 1 {
				return fmt.Errorf("%w: missing padding byte after %s sub chunk", ErrTruncatedChunk, subID)
			}
			var pad byte
			if err := binary.Read(r, binary.LittleEndian, &pad); err != nil {
				return fmt.Errorf("failed to read the %s padding byte - %w", subID, err)
			}
			remaining--
		}

		switch subID {
		case markerLabl:
			cueID, text, err := decodeCueText(payload)
			if err != nil {
				return fmt.Errorf("labl: %w", err)
			}
			if seenLabel[cueID] {
				return fmt.Errorf("%w %s: conflicting labl entries", ErrDuplicateAdtlEntry, fourCC(cueID))
			}
			seenLabel[cueID] = true
			md.CueLabels = append(md.CueLabels, &CueLabel{CueID: cueID, Text: text})
		case markerNote:
			cueID, text, err := decodeCueText(payload)
			if err != nil {
				return fmt.Errorf("note: %w", err)
			}
			if seenNote[cueID] {
				return fmt.Errorf("%w %s: conflicting note entries", ErrDuplicateAdtlEntry, fourCC(cueID))
			}
			seenNote[cueID] = true
			md.CueNotes = append(md.CueNotes, &CueNote{CueID: cueID, Text: text})
		case markerLtxt:
			seg, err := decodeLtxt(payload)
			if err != nil {
				return fmt.Errorf("ltxt: %w", err)
			}
			if seenSegment[seg.CueID] {
				return fmt.Errorf("%w %s: conflicting ltxt entries", ErrDuplicateAdtlEntry, fourCC(seg.CueID))
			}
			seenSegment[seg.CueID] = true
			md.CueSegments = append(md.CueSegments, seg)
		default:
			// unknown adtl sub chunks (e.g. "rfrl") are skipped but their
			// bytes have already been consumed above.
		}
	}
	return nil
}

// decodeCueText decodes the shared payload of labl and note sub chunks:
// a 4 byte cue ID followed by a null terminated text string.
func decodeCueText(payload []byte) (cueID [4]byte, text string, err error) {
	if len(payload) < 5 {
		return cueID, "", fmt.Errorf("%w: cue text sub chunk must hold an ID and a null terminated string, got %d bytes",
			ErrTruncatedChunk, len(payload))
	}
	copy(cueID[:], payload[:4])
	// A well formed file terminates the string with a null byte; tolerate a
	// missing terminator by using the whole remaining payload.
	text = nullTermStr(payload[4:])
	return cueID, text, nil
}

// decodeLtxt decodes an ltxt sub chunk payload.
func decodeLtxt(payload []byte) (*CueSegment, error) {
	if len(payload) < ltxtFixedFields {
		return nil, fmt.Errorf("%w: ltxt sub chunk needs at least %d bytes, got %d",
			ErrTruncatedChunk, ltxtFixedFields, len(payload))
	}
	seg := &CueSegment{}
	copy(seg.CueID[:], payload[:4])
	seg.SampleLength = binary.LittleEndian.Uint32(payload[4:8])
	copy(seg.Purpose[:], payload[8:12])
	seg.Country = binary.LittleEndian.Uint16(payload[12:14])
	seg.Language = binary.LittleEndian.Uint16(payload[14:16])
	seg.Dialect = binary.LittleEndian.Uint16(payload[16:18])
	seg.CodePage = binary.LittleEndian.Uint16(payload[18:20])
	if len(payload) > ltxtFixedFields {
		seg.Text = nullTermStr(payload[ltxtFixedFields:])
	}
	return seg, nil
}

// Validate cross references the decoded cue point annotations against the cue
// points and the audio content. It checks that every labl/note/ltxt entry
// targets a known cue ID, that no entry type is declared twice for the same
// cue ID and that labeled segments stay within the PCM data.
//
// totalFrames is the total number of audio frames in the data chunk; pass 0
// when the length is unknown, in which case segment bounds are not checked.
func (md *Metadata) Validate(totalFrames uint64) error {
	if md == nil {
		return nil
	}
	cueKnown := map[[4]byte]bool{}
	for _, c := range md.CuePoints {
		if c == nil {
			continue
		}
		if cueKnown[c.ID] {
			return fmt.Errorf("%w %s in cue chunk", ErrDuplicateAdtlEntry, fourCC(c.ID))
		}
		cueKnown[c.ID] = true
	}

	seen := map[[4]byte]map[byte]bool{}
	noteConflict := func(id [4]byte, kind byte) error {
		if _, ok := seen[id]; !ok {
			seen[id] = map[byte]bool{}
		}
		if seen[id][kind] {
			return fmt.Errorf("%w %s", ErrDuplicateAdtlEntry, fourCC(id))
		}
		seen[id][kind] = true
		return nil
	}

	const (
		kindLabel = iota + 1
		kindNote
		kindSegment
	)

	for _, l := range md.CueLabels {
		if l == nil {
			continue
		}
		if err := noteConflict(l.CueID, kindLabel); err != nil {
			return err
		}
		if !cueKnown[l.CueID] {
			return fmt.Errorf("%w %s in labl entry", ErrUnknownCueID, fourCC(l.CueID))
		}
	}
	for _, n := range md.CueNotes {
		if n == nil {
			continue
		}
		if err := noteConflict(n.CueID, kindNote); err != nil {
			return err
		}
		if !cueKnown[n.CueID] {
			return fmt.Errorf("%w %s in note entry", ErrUnknownCueID, fourCC(n.CueID))
		}
	}
	for _, s := range md.CueSegments {
		if s == nil {
			continue
		}
		if err := noteConflict(s.CueID, kindSegment); err != nil {
			return err
		}
		if !cueKnown[s.CueID] {
			return fmt.Errorf("%w %s in ltxt entry", ErrUnknownCueID, fourCC(s.CueID))
		}
		if totalFrames > 0 {
			start, ok := md.cueFrame(s.CueID)
			if !ok {
				return fmt.Errorf("%w %s in ltxt entry", ErrUnknownCueID, fourCC(s.CueID))
			}
			length := uint64(s.SampleLength)
			// guard against start + length overflowing a uint64
			if length > totalFrames || start > totalFrames-length {
				return fmt.Errorf("%w: segment of cue %s ends at frame %d but only %d frames are available",
					ErrSegmentOutOfRange, fourCC(s.CueID), start+length, totalFrames)
			}
		}
	}
	return nil
}

// cueFrame returns the sample frame position of the cue point with the passed
// ID. For PCM data Position and SampleOffset carry the same value; Position is
// preferred when set.
func (md *Metadata) cueFrame(id [4]byte) (uint64, bool) {
	for _, c := range md.CuePoints {
		if c == nil || c.ID != id {
			continue
		}
		if c.Position > 0 {
			return uint64(c.Position), true
		}
		return uint64(c.SampleOffset), true
	}
	return 0, false
}

// fourCC renders a four character code for error messages.
func fourCC(id [4]byte) string {
	return fmt.Sprintf("0x%02x%02x%02x%02x", id[0], id[1], id[2], id[3])
}

// checkText validates a text string about to be written in a labl/note/ltxt
// sub chunk: the sub chunk size field is 32 bit and the strings are stored as
// UTF-8 terminated by a null byte.
func checkText(text string, withNull bool) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("%w: %q", ErrInvalidUTF8, text)
	}
	size := uint64(len(text))
	if withNull {
		size++
	}
	if size > maxChunkSize {
		return fmt.Errorf("%w: %d bytes", ErrTextTooLong, size)
	}
	return nil
}
