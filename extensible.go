package wav

import (
	"errors"
	"fmt"
	"math/bits"

	"github.com/go-audio/riff"
)

// WAVE format tags stored in the wFormatTag field of the "fmt " chunk.
const (
	// WAVFormatPCM is the standard uncompressed integer PCM format tag.
	WAVFormatPCM uint16 = 1
	// WAVFormatExtensible is the WAVE_FORMAT_EXTENSIBLE tag. Files using it
	// carry an extended "fmt " chunk describing the speaker layout, the actual
	// number of valid bits per sample and a sub-format GUID.
	WAVFormatExtensible uint16 = 0xFFFE
)

// cbSize value of an extended "fmt " chunk carrying the full
// WAVE_FORMAT_EXTENSIBLE extension (40 bytes total: 16 standard + 24 extra).
const extensibleFmtExtraSize uint16 = 22

// Standard speaker position bits used in the dwChannelMask field.
// The mask describes which speaker position each interleaved channel maps to,
// from the least significant bit upward.
const (
	SpeakerFL  uint32 = 0x00000001 // front left
	SpeakerFR  uint32 = 0x00000002 // front right
	SpeakerFC  uint32 = 0x00000004 // front center
	SpeakerLFE uint32 = 0x00000008 // low frequency effects (subwoofer)
	SpeakerBL  uint32 = 0x00000010 // back left
	SpeakerBR  uint32 = 0x00000020 // back right
	SpeakerFLC uint32 = 0x00000040 // front left of center
	SpeakerFRC uint32 = 0x00000080 // front right of center
	SpeakerBC  uint32 = 0x00000100 // back center
	SpeakerSL  uint32 = 0x00000200 // side left
	SpeakerSR  uint32 = 0x00000400 // side right
)

// Common channel masks.
const (
	// ChannelMaskMono is the usual mask for a single center channel.
	ChannelMaskMono uint32 = SpeakerFC
	// ChannelMaskStereo is the usual mask for a front left / front right pair.
	ChannelMaskStereo uint32 = SpeakerFL | SpeakerFR
	// ChannelMask5Point1 is the standard 5.1 layout: FL, FR, FC, LFE, BL, BR.
	ChannelMask5Point1 uint32 = SpeakerFL | SpeakerFR | SpeakerFC | SpeakerLFE | SpeakerBL | SpeakerBR
)

var (
	// ErrFmtChunkTruncated indicates the "fmt " chunk is too short to hold the
	// fields it claims to contain.
	ErrFmtChunkTruncated = errors.New("wav: truncated fmt chunk")
	// ErrChannelMaskMismatch indicates the number of bits set in the channel
	// mask does not match the number of channels.
	ErrChannelMaskMismatch = errors.New("wav: channel mask bit count does not match the number of channels")
	// ErrValidBitsExceedsContainer indicates wValidBitsPerSample is larger than
	// the container size given by wBitsPerSample.
	ErrValidBitsExceedsContainer = errors.New("wav: valid bits per sample exceed the container bit depth")
	// ErrUnknownSubFormat indicates a WAVE_FORMAT_EXTENSIBLE file uses a
	// sub-format this package does not understand. Only integer PCM
	// (KSDATAFORMAT_SUBTYPE_PCM) is supported.
	ErrUnknownSubFormat = errors.New("wav: unknown WAVE_FORMAT_EXTENSIBLE sub-format")
)

// GUID mirrors the Windows GUID structure used by the
// WAVE_FORMAT_EXTENSIBLE sub-format field. Its binary representation is the
// 16 byte layout found on disk: Data1 (LE u32), Data2 (LE u16),
// Data3 (LE u16), Data4 (8 raw bytes).
type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// PCMSubFormatGUID is KSDATAFORMAT_SUBTYPE_PCM, the sub-format of an
// extensible file holding uncompressed integer PCM samples.
//
//	00000001-0000-0010-8000-00aa00389b71
var PCMSubFormatGUID = GUID{
	Data1: 0x00000001,
	Data2: 0x0000,
	Data3: 0x0010,
	Data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71},
}

// String renders the GUID in the canonical registry form
// (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
func (g GUID) String() string {
	return fmt.Sprintf("%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		g.Data1, g.Data2, g.Data3,
		g.Data4[0], g.Data4[1], g.Data4[2], g.Data4[3],
		g.Data4[4], g.Data4[5], g.Data4[6], g.Data4[7])
}

// DefaultChannelMask returns a standard channel mask for common channel
// counts: 1 -> mono (front center), 2 -> stereo (FL/FR), 6 -> 5.1
// (FL/FR/FC/LFE/BL/BR). It returns 0 for counts without a built-in default.
func DefaultChannelMask(numChans int) uint32 {
	switch numChans {
	case 1:
		return ChannelMaskMono
	case 2:
		return ChannelMaskStereo
	case 6:
		return ChannelMask5Point1
	default:
		return 0
	}
}

// parseFmtChunk decodes a "fmt " chunk, including the WAVE_FORMAT_EXTENSIBLE
// extension, and populates the decoder and underlying riff parser fields.
// The whole chunk is always drained before returning so the next chunk can be
// read regardless of any validation error.
func (d *Decoder) parseFmtChunk(chunk *riff.Chunk) error {
	// A "fmt " chunk must at least contain the 16 standard PCM fields.
	if chunk.Size < 16 {
		chunk.Drain()
		return fmt.Errorf("%w: chunk is %d bytes, need at least 16", ErrFmtChunkTruncated, chunk.Size)
	}

	var (
		audioFormat    uint16
		numChannels    uint16
		sampleRate     uint32
		avgBytesPerSec uint32
		blockAlign     uint16
		bitsPerSample  uint16
	)

	readErr := func(err error) error {
		chunk.Drain()
		return fmt.Errorf("%w: %v", ErrFmtChunkTruncated, err)
	}

	if err := chunk.ReadLE(&audioFormat); err != nil {
		return readErr(err)
	}
	if err := chunk.ReadLE(&numChannels); err != nil {
		return readErr(err)
	}
	if err := chunk.ReadLE(&sampleRate); err != nil {
		return readErr(err)
	}
	if err := chunk.ReadLE(&avgBytesPerSec); err != nil {
		return readErr(err)
	}
	if err := chunk.ReadLE(&blockAlign); err != nil {
		return readErr(err)
	}
	if err := chunk.ReadLE(&bitsPerSample); err != nil {
		return readErr(err)
	}

	d.parser.WavAudioFormat = audioFormat
	d.parser.NumChannels = numChannels
	d.parser.SampleRate = sampleRate
	d.parser.AvgBytesPerSec = avgBytesPerSec
	d.parser.BlockAlign = blockAlign
	d.parser.BitsPerSample = bitsPerSample

	d.NumChans = numChannels
	d.SampleRate = sampleRate
	d.AvgBytesPerSec = avgBytesPerSec
	d.WavAudioFormat = audioFormat
	d.BitDepth = bitsPerSample

	// Reset extension fields so a standard "fmt " chunk never exposes values
	// left over from a previous parse (e.g. after Rewind on a different file).
	d.IsExtensible = false
	d.ValidBitsPerSample = 0
	d.ChannelMask = 0
	d.SubFormat = GUID{}

	if audioFormat != WAVFormatExtensible {
		// Any trailing bytes are codec specific data we don't interpret.
		chunk.Drain()
		return nil
	}

	// The extension needs cbSize (2) + valid bits (2) + channel mask (4) +
	// GUID (16) = 24 extra bytes, i.e. a 40 byte chunk.
	if chunk.Size < 16+2+2+4+16 {
		chunk.Drain()
		return fmt.Errorf("%w: extensible fmt chunk is %d bytes, need at least 40", ErrFmtChunkTruncated, chunk.Size)
	}

	var cbSize uint16
	if err := chunk.ReadLE(&cbSize); err != nil {
		return readErr(err)
	}
	// The WAVE_FORMAT_EXTENSIBLE extension is fixed-size: cbSize must be at
	// least 22 (the size of the extension). Larger values are tolerated, the
	// trailing vendor bytes being ignored, but cbSize cannot claim more bytes
	// than the chunk actually contains.
	if cbSize < extensibleFmtExtraSize {
		chunk.Drain()
		return fmt.Errorf("%w: cbSize is %d, WAVE_FORMAT_EXTENSIBLE requires at least %d",
			ErrFmtChunkTruncated, cbSize, extensibleFmtExtraSize)
	}
	// The chunk holds 16 standard bytes plus cbSize extension bytes.
	if int(cbSize)+16 > chunk.Size {
		chunk.Drain()
		return fmt.Errorf("%w: cbSize %d exceeds the fmt chunk size of %d",
			ErrFmtChunkTruncated, cbSize, chunk.Size)
	}

	var validBitsPerSample uint16
	if err := chunk.ReadLE(&validBitsPerSample); err != nil {
		return readErr(err)
	}
	var channelMask uint32
	if err := chunk.ReadLE(&channelMask); err != nil {
		return readErr(err)
	}
	var subFormat GUID
	if err := chunk.ReadLE(&subFormat); err != nil {
		return readErr(err)
	}

	// Anything beyond the canonical 40 bytes is ignored.
	chunk.Drain()

	if subFormat != PCMSubFormatGUID {
		return fmt.Errorf("%w: %s", ErrUnknownSubFormat, subFormat)
	}
	if validBitsPerSample > bitsPerSample {
		return fmt.Errorf("%w: %d valid bits in a %d bit container",
			ErrValidBitsExceedsContainer, validBitsPerSample, bitsPerSample)
	}
	if got := bits.OnesCount32(channelMask); got != int(numChannels) {
		return fmt.Errorf("%w: mask %#010x sets %d bits but the file has %d channels",
			ErrChannelMaskMismatch, channelMask, got, numChannels)
	}

	d.IsExtensible = true
	d.ValidBitsPerSample = validBitsPerSample
	d.ChannelMask = channelMask
	d.SubFormat = subFormat

	return nil
}

// writeExtensibleFmt writes the 24 WAVE_FORMAT_EXTENSIBLE extension bytes
// following the 16 standard "fmt " fields.
func (e *Encoder) writeExtensibleFmt() error {
	validBits := e.ValidBitsPerSample
	if validBits == 0 {
		validBits = e.BitDepth
	}

	if err := e.AddLE(extensibleFmtExtraSize); err != nil { // cbSize
		return err
	}
	if err := e.AddLE(uint16(validBits)); err != nil { // wValidBitsPerSample
		return err
	}
	if err := e.AddLE(uint32(e.ChannelMask)); err != nil { // dwChannelMask
		return err
	}
	// KSDATAFORMAT_SUBTYPE_PCM GUID, 16 bytes in on-disk layout.
	if err := e.AddLE(PCMSubFormatGUID.Data1); err != nil {
		return err
	}
	if err := e.AddLE(PCMSubFormatGUID.Data2); err != nil {
		return err
	}
	if err := e.AddLE(PCMSubFormatGUID.Data3); err != nil {
		return err
	}
	for i := range PCMSubFormatGUID.Data4 {
		if err := e.AddLE(PCMSubFormatGUID.Data4[i]); err != nil {
			return err
		}
	}
	return nil
}
