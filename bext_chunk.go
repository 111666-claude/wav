package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"time"

	"github.com/go-audio/riff"
)

// CIDBext is the chunk ID for a Broadcast Wave Format bext chunk.
// See EBU Tech 3285 for the full specification.
var CIDBext = [4]byte{'b', 'e', 'x', 't'}

// Fixed field sizes of a Broadcast Wave Format version 0 bext chunk.
const (
	bextDescriptionSize         = 256
	bextOriginatorSize          = 32
	bextOriginatorReferenceSize = 32
	bextOriginationDateSize     = 10
	bextOriginationTimeSize     = 8
	bextTimeReferenceSize       = 8
	bextVersionSize             = 2
	// bextV0FixedSize is the size of the bext chunk when CodingHistory is empty.
	bextV0FixedSize = bextDescriptionSize + bextOriginatorSize +
		bextOriginatorReferenceSize + bextOriginationDateSize +
		bextOriginationTimeSize + bextTimeReferenceSize + bextVersionSize
	bextUMIDSize = 64 // present in version 1 and above
	// bextV2ExtraSize covers the five int16 loudness fields added in
	// version 2: LoudnessValue, LoudnessRange, MaxTruePeakLevel,
	// MaxMomentaryLoudness and MaxShortTermLoudness.
	bextV2ExtraSize = 10
)

var (
	// ErrBextFieldTooLong is returned when a bext string field does not fit in
	// its fixed size slot. The offending field is never silently truncated.
	ErrBextFieldTooLong = errors.New("wav: bext field too long for its fixed size slot")
	// ErrBextInvalidDate is returned when OriginationDate is not formatted as
	// YYYY-MM-DD.
	ErrBextInvalidDate = errors.New("wav: bext origination date must use the YYYY-MM-DD format")
	// ErrBextInvalidTime is returned when OriginationTime is not formatted as
	// HH:MM:SS.
	ErrBextInvalidTime = errors.New("wav: bext origination time must use the HH:MM:SS format")
	// ErrBextTimeReferenceOutOfRange is returned when a time reference cannot be
	// represented in the unsigned 64 bit TimeReference field.
	ErrBextTimeReferenceOutOfRange = errors.New("wav: bext time reference cannot be represented as an unsigned 64 bit value")
	// ErrBextTruncated is returned when a bext chunk ends before its fixed size
	// fields have been fully read.
	ErrBextTruncated = errors.New("wav: bext chunk is truncated")
)

// Bext holds the fields of a Broadcast Wave Format bext chunk.
// All string fields except CodingHistory are written into fixed size, NUL
// padded slots; values longer than their slot make encoding fail instead of
// being truncated.
type Bext struct {
	// Description is a free form description of the audio sequence, up to 255
	// bytes plus the mandatory NUL terminator.
	Description string
	// Originator identifies the creator of the file, up to 31 bytes plus the
	// mandatory NUL terminator.
	Originator string
	// OriginatorReference is an unambiguous reference allocated by the
	// originating organisation, up to 31 bytes plus the mandatory NUL
	// terminator.
	OriginatorReference string
	// OriginationDate is the creation date in YYYY-MM-DD format.
	OriginationDate string
	// OriginationTime is the creation time in 24 hour HH:MM:SS format.
	OriginationTime string
	// TimeReference is the first sample count since midnight, an unsigned 64
	// bit value.
	TimeReference uint64
	// CodingHistory is a variable size, CR/LF terminated list of coding
	// algorithms applied to the audio data.
	CodingHistory string
	// Version is the BWF version read from the chunk (0, 1 or 2). Encoded
	// chunks are always written as version 0.
	Version uint16
}

// SetTimeReference computes the unsigned 64 bit TimeReference from a duration
// since midnight and the sample rate of the audio. An error wrapping
// ErrBextTimeReferenceOutOfRange is returned for negative durations or values
// that do not fit in 64 bits.
func (b *Bext) SetTimeReference(sinceMidnight time.Duration, sampleRate uint64) error {
	ref, err := TimeReferenceFromDuration(sinceMidnight, sampleRate)
	if err != nil {
		return err
	}
	b.TimeReference = ref
	return nil
}

// TimeReferenceFromDuration converts a duration since midnight and a sample
// rate into the unsigned 64 bit sample count stored in the bext chunk. It
// returns an error wrapping ErrBextTimeReferenceOutOfRange for negative
// durations, a zero sample rate or values overflowing 64 bits.
func TimeReferenceFromDuration(sinceMidnight time.Duration, sampleRate uint64) (uint64, error) {
	if sampleRate == 0 {
		return 0, fmt.Errorf("%w: sample rate must be greater than 0", ErrBextTimeReferenceOutOfRange)
	}
	if sinceMidnight < 0 {
		return 0, fmt.Errorf("%w: negative duration %s", ErrBextTimeReferenceOutOfRange, sinceMidnight)
	}
	// sinceMidnight = (whole seconds + fraction of a second)
	secs := uint64(sinceMidnight / time.Second)
	nanos := uint64(sinceMidnight % time.Second)
	hi, lo := bits.Mul64(secs, sampleRate)
	// fraction of a second rounded down to whole samples
	var fraction uint64
	if nanos > 0 {
		fhi, flo := bits.Mul64(nanos, sampleRate)
		if fhi != 0 {
			// fhi < 1e9 (nanos < 1e9) so the quotient always fits in 64 bits.
			fraction, _ = bits.Div64(fhi, flo, uint64(time.Second))
		} else {
			fraction = flo / uint64(time.Second)
		}
	}
	sum, carry := bits.Add64(lo, fraction, 0)
	if hi != 0 || carry != 0 {
		return 0, fmt.Errorf("%w: %s at %d Hz overflows 64 bits", ErrBextTimeReferenceOutOfRange, sinceMidnight, sampleRate)
	}
	return sum, nil
}

// writeFixedString writes val into a size bytes slot, NUL padding the unused
// space. It returns an error wrapping ErrBextFieldTooLong if the value does not
// fit together with its NUL terminator.
func writeFixedString(buf *bytes.Buffer, field, val string, size int) error {
	return writeSlot(buf, field, val, size, true)
}

// writeFixedSlot writes val into a size bytes slot, NUL padding the unused
// space. When requireNUL is false the value is allowed to fill the whole slot,
// as mandated for the fixed width OriginationDate and OriginationTime fields.
func writeSlot(buf *bytes.Buffer, field, val string, size int, requireNUL bool) error {
	limit := size
	if requireNUL {
		limit = size - 1
	}
	if len(val) > limit {
		return fmt.Errorf("%w: %s is %d bytes, slot is %d bytes", ErrBextFieldTooLong, field, len(val), size)
	}
	slot := make([]byte, size)
	copy(slot, val)
	// any byte after the content stays 0, acting as the NUL terminator
	_, err := buf.Write(slot)
	return err
}

// encodeBextChunk serializes b into the payload of a bext chunk (the chunk ID
// and chunk size header are not included). The returned payload is always the
// BWF version 0 layout: fixed size fields followed by CodingHistory.
func encodeBextChunk(b *Bext) ([]byte, error) {
	if err := validateBext(b); err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(make([]byte, 0, bextV0FixedSize+len(b.CodingHistory)))
	if err := writeFixedString(buf, "Description", b.Description, bextDescriptionSize); err != nil {
		return nil, err
	}
	if err := writeFixedString(buf, "Originator", b.Originator, bextOriginatorSize); err != nil {
		return nil, err
	}
	if err := writeFixedString(buf, "OriginatorReference", b.OriginatorReference, bextOriginatorReferenceSize); err != nil {
		return nil, err
	}
	// the date and time fields are fixed width and need no NUL terminator
	if err := writeSlot(buf, "OriginationDate", b.OriginationDate, bextOriginationDateSize, false); err != nil {
		return nil, err
	}
	if err := writeSlot(buf, "OriginationTime", b.OriginationTime, bextOriginationTimeSize, false); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, b.TimeReference); err != nil {
		return nil, err
	}
	// BWF version 0, little endian.
	if err := binary.Write(buf, binary.LittleEndian, uint16(0)); err != nil {
		return nil, err
	}
	buf.WriteString(b.CodingHistory)

	return buf.Bytes(), nil
}

func validateBext(b *Bext) error {
	if b == nil {
		return nil
	}
	type fixedField struct {
		name       string
		val        string
		size       int
		requireNUL bool
	}
	for _, f := range []fixedField{
		{"Description", b.Description, bextDescriptionSize, true},
		{"Originator", b.Originator, bextOriginatorSize, true},
		{"OriginatorReference", b.OriginatorReference, bextOriginatorReferenceSize, true},
		{"OriginationDate", b.OriginationDate, bextOriginationDateSize, false},
		{"OriginationTime", b.OriginationTime, bextOriginationTimeSize, false},
	} {
		limit := f.size
		if f.requireNUL {
			limit--
		}
		if len(f.val) > limit {
			return fmt.Errorf("%w: %s is %d bytes, slot is %d bytes", ErrBextFieldTooLong, f.name, len(f.val), f.size)
		}
	}
	if b.OriginationDate != "" {
		if _, err := time.Parse("2006-01-02", b.OriginationDate); err != nil {
			return fmt.Errorf("%w: %q: %v", ErrBextInvalidDate, b.OriginationDate, err)
		}
	}
	if b.OriginationTime != "" {
		if _, err := time.Parse("15:04:05", b.OriginationTime); err != nil {
			return fmt.Errorf("%w: %q: %v", ErrBextInvalidTime, b.OriginationTime, err)
		}
	}
	if uint64(len(b.CodingHistory))+uint64(bextV0FixedSize) > uint64(^uint32(0)) {
		return fmt.Errorf("%w: CodingHistory is %d bytes, the bext chunk would exceed the 32 bit chunk size", ErrBextFieldTooLong, len(b.CodingHistory))
	}
	return nil
}

// DecodeBextChunk decodes a bext chunk into a Bext structure. BWF versions 0,
// 1 and 2 are accepted; the UMID and loudness extension fields are skipped so
// that CodingHistory is exposed correctly for every version.
func DecodeBextChunk(d *Decoder, ch *riff.Chunk) error {
	if ch == nil {
		return fmt.Errorf("can't decode a nil chunk")
	}
	if d == nil {
		return fmt.Errorf("nil decoder")
	}
	if ch.ID != CIDBext {
		ch.Drain()
		return nil
	}

	if ch.Size < bextV0FixedSize {
		ch.Drain()
		return fmt.Errorf("%w: chunk is %d bytes but the fixed bext fields need %d bytes", ErrBextTruncated, ch.Size, bextV0FixedSize)
	}

	// read the entire chunk in memory; ch.Read keeps ch.Pos in sync with the
	// underlying reader so the final Drain is a no-op and never eats into the
	// next chunk.
	raw := make([]byte, ch.Size)
	if _, err := ch.Read(raw); err != nil {
		ch.Drain()
		return fmt.Errorf("%w: %v", ErrBextTruncated, err)
	}

	b := &Bext{}
	b.Description = nullTermStr(raw[0:bextDescriptionSize])
	off := bextDescriptionSize
	b.Originator = nullTermStr(raw[off : off+bextOriginatorSize])
	off += bextOriginatorSize
	b.OriginatorReference = nullTermStr(raw[off : off+bextOriginatorReferenceSize])
	off += bextOriginatorReferenceSize
	b.OriginationDate = nullTermStr(raw[off : off+bextOriginationDateSize])
	off += bextOriginationDateSize
	b.OriginationTime = nullTermStr(raw[off : off+bextOriginationTimeSize])
	off += bextOriginationTimeSize
	b.TimeReference = binary.LittleEndian.Uint64(raw[off : off+bextTimeReferenceSize])
	off += bextTimeReferenceSize
	b.Version = binary.LittleEndian.Uint16(raw[off : off+bextVersionSize])
	off += bextVersionSize

	// Version 1 adds a 64 byte UMID, version 2 adds another 10 bytes of
	// loudness fields before CodingHistory.
	skip := 0
	switch {
	case b.Version >= 2:
		skip = bextUMIDSize + bextV2ExtraSize
	case b.Version == 1:
		skip = bextUMIDSize
	}
	if skip > len(raw)-off {
		ch.Drain()
		return fmt.Errorf("%w: %d byte version %d extension does not fit in the remaining %d bytes", ErrBextTruncated, skip, b.Version, len(raw)-off)
	}
	off += skip
	b.CodingHistory = string(bytes.TrimRight(raw[off:], "\x00"))

	d.Bext = b
	ch.Drain()
	return nil
}
