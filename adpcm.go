package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// WavFormatIMAADPCM is the WAVE format tag for IMA ADPCM encoded data.
const WavFormatIMAADPCM uint16 = 0x0011

var (
	// ErrInvalidBlockAlign indicates that the fmt block align value is not
	// compatible with IMA ADPCM and the declared number of channels.
	ErrInvalidBlockAlign = errors.New("wav: invalid IMA ADPCM block align")
	// ErrTruncatedBlock indicates that an ADPCM block ended before all the
	// bytes declared by the block align value could be read.
	ErrTruncatedBlock = errors.New("wav: truncated IMA ADPCM block")
	// ErrStepIndexOutOfRange indicates that a block header carried an
	// initial step index outside of the valid [0, 88] range.
	ErrStepIndexOutOfRange = errors.New("wav: IMA ADPCM step index out of range")
	// ErrFactMismatch indicates that the number of decoded samples does not
	// match the sample count advertised by the fact chunk.
	ErrFactMismatch = errors.New("wav: fact chunk sample count does not match the data chunk")
)

// imaStepTable holds the quantizer step sizes used by the IMA ADPCM
// algorithm. These are the exact Intel DVI/ADPCM reference values, as used
// by the WAV IMA ADPCM decoders shipped with Windows and ffmpeg. The table
// diverges from the rounded IMA recommendation from index 50 onward.
var imaStepTable = [89]int{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17,
	19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118,
	130, 143, 157, 173, 190, 209, 230, 253, 279, 307,
	337, 371, 408, 449, 494, 544, 598, 658, 724, 796,
	876, 963, 1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066,
	2272, 2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871, 5358,
	5894, 6484, 7132, 7845, 8630, 9493, 10442, 11487, 12635, 13899,
	15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767,
}

// imaIndexTable maps the 4 bit code word to the step index adjustment.
var imaIndexTable = [16]int{
	-1, -1, -1, -1, 2, 4, 6, 8,
	-1, -1, -1, -1, 2, 4, 6, 8,
}

// imaChannelDecoder tracks the running state of one IMA ADPCM channel
// inside a block.
type imaChannelDecoder struct {
	predictor int
	stepIndex int
}

// reset initializes the channel state from the 4 byte block header
// (first predictor sample + step index) as stored in an IMA ADPCM block.
func (c *imaChannelDecoder) reset(header []byte) error {
	if len(header) < 4 {
		return io.ErrUnexpectedEOF
	}
	c.predictor = int(int16(binary.LittleEndian.Uint16(header[0:2])))
	c.stepIndex = int(header[2])
	// header[3] is a reserved byte that must be zero.
	if c.stepIndex > 88 {
		return fmt.Errorf("%w: got %d, valid range is 0-88", ErrStepIndexOutOfRange, c.stepIndex)
	}
	return nil
}

// decodeNibble expands one 4 bit ADPCM code word into a 16 bit sample.
func (c *imaChannelDecoder) decodeNibble(code int) int {
	step := imaStepTable[c.stepIndex]
	// Reconstruct the difference with the direct multiplication form from
	// the Intel DVI/ADPCM reference (also used by the WAV IMA decoders in
	// Windows and ffmpeg). Bit 3 is the sign bit, bits 2..0 the magnitude.
	delta := code & 0x07
	diff := ((2*delta + 1) * step) >> 3
	if code&0x08 != 0 {
		c.predictor -= diff
	} else {
		c.predictor += diff
	}

	// Clamp the predictor to the signed 16 bit range.
	if c.predictor > 32767 {
		c.predictor = 32767
	} else if c.predictor < -32768 {
		c.predictor = -32768
	}

	// Update the step index and clamp it as well.
	c.stepIndex += imaIndexTable[code]
	if c.stepIndex < 0 {
		c.stepIndex = 0
	} else if c.stepIndex > 88 {
		c.stepIndex = 88
	}
	return c.predictor
}

// samplesPerADPCMBlock returns the number of samples (per channel) stored
// in one IMA ADPCM block.
func samplesPerADPCMBlock(blockAlign, numChans int) int {
	if numChans == 2 {
		// A stereo block holds two 4 byte headers followed by groups of
		// 8 bytes: four nibble packed bytes for the left channel then
		// four for the right channel. Each group yields 8 samples per
		// channel.
		return (blockAlign-8)/8*8 + 1
	}
	// A mono block starts with a 4 byte header, every remaining byte holds
	// two 4 bit samples.
	return (blockAlign-4)*2 + 1
}

// adpcmReader decodes IMA ADPCM blocks from the data chunk reader into the
// interleaved 16 bit PCM representation used by audio.IntBuffer.
type adpcmReader struct {
	r          io.Reader
	numChans   int
	blockAlign int
	// samplesPerBlock is the per-channel sample capacity of a full block.
	samplesPerBlock int
	// factSamples is the total per-channel sample count advertised by the
	// fact chunk, or 0 when no fact chunk was present.
	factSamples uint32

	// framesEmitted counts the decoded frames (one sample per channel).
	framesEmitted int
	// pending holds decoded interleaved samples not yet consumed.
	pending []int
	// blockBuf is reused across block reads.
	blockBuf []byte
	// done is set once the data chunk has been fully consumed.
	done bool
}

// newADPCMReader validates the format parameters and wraps the data chunk.
func newADPCMReader(r io.Reader, numChans, blockAlign int, factSamples uint32) (*adpcmReader, error) {
	if numChans != 1 && numChans != 2 {
		return nil, fmt.Errorf("%w: IMA ADPCM supports 1 or 2 channels, got %d", ErrInvalidBlockAlign, numChans)
	}
	if numChans == 1 && blockAlign < 4 {
		return nil, fmt.Errorf("%w: mono block align must be at least 4 bytes, got %d", ErrInvalidBlockAlign, blockAlign)
	}
	if numChans == 2 && blockAlign < 8 {
		return nil, fmt.Errorf("%w: stereo block align must be at least 8 bytes, got %d", ErrInvalidBlockAlign, blockAlign)
	}
	if numChans == 2 && (blockAlign-8)%8 != 0 {
		return nil, fmt.Errorf("%w: stereo block align %d must be 8 plus a multiple of 8", ErrInvalidBlockAlign, blockAlign)
	}
	return &adpcmReader{
		r:               r,
		numChans:        numChans,
		blockAlign:      blockAlign,
		samplesPerBlock: samplesPerADPCMBlock(blockAlign, numChans),
		factSamples:     factSamples,
		blockBuf:        make([]byte, blockAlign),
	}, nil
}

// decodeMonoBlock decodes one mono block into out, returning the number of
// samples written (including the header predictor sample).
func (ar *adpcmReader) decodeMonoBlock(block []byte, out []int) (int, error) {
	var dec imaChannelDecoder
	if err := dec.reset(block[:4]); err != nil {
		return 0, err
	}
	n := 0
	out[n] = dec.predictor
	n++
	for _, b := range block[4:] {
		if n >= len(out) {
			break
		}
		out[n] = dec.decodeNibble(int(b & 0x0F))
		n++
		if n >= len(out) {
			break
		}
		out[n] = dec.decodeNibble(int(b >> 4))
		n++
	}
	return n, nil
}

// decodeStereoBlock decodes one interleaved stereo block. The block starts
// with both 4 byte channel headers, followed by groups of 8 bytes: four
// nibble packed bytes for the left channel then four for the right channel.
// Within each byte the low nibble is decoded before the high nibble.
func (ar *adpcmReader) decodeStereoBlock(block []byte, out []int) (int, error) {
	var left, right imaChannelDecoder
	if err := left.reset(block[0:4]); err != nil {
		return 0, fmt.Errorf("left channel: %w", err)
	}
	if err := right.reset(block[4:8]); err != nil {
		return 0, fmt.Errorf("right channel: %w", err)
	}
	n := 0
	// Frame 0 comes straight from the block headers.
	out[n] = left.predictor
	out[n+1] = right.predictor
	n += 2
	for off := 8; off+7 < len(block); off += 8 {
		leftBytes := block[off : off+4]
		rightBytes := block[off+4 : off+8]
		// Decode both channels sample by sample so the output stays frame
		// interleaved (left, right, left, right, ...). Each channel byte
		// contributes a low then a high nibble, i.e. two samples.
		for j := 0; j < 8; j++ {
			b := j / 2
			var leftCode, rightCode int
			if j&1 == 0 {
				leftCode = int(leftBytes[b] & 0x0F)
				rightCode = int(rightBytes[b] & 0x0F)
			} else {
				leftCode = int(leftBytes[b] >> 4)
				rightCode = int(rightBytes[b] >> 4)
			}
			out[n] = left.decodeNibble(leftCode)
			out[n+1] = right.decodeNibble(rightCode)
			n += 2
		}
	}
	return n, nil
}

// readBlock decodes the next block and stores its interleaved samples in
// pending. io.EOF is returned without decoding when no block remains. A
// partial final block is reported as ErrTruncatedBlock.
func (ar *adpcmReader) readBlock() error {
	n, err := io.ReadFull(ar.r, ar.blockBuf)
	if err == io.EOF {
		// Clean end of the data chunk.
		ar.done = true
		return io.EOF
	}
	if err != nil {
		// io.ErrUnexpectedEOF means a truncated final block.
		if err == io.ErrUnexpectedEOF {
			return fmt.Errorf("%w: read %d of %d bytes", ErrTruncatedBlock, n, ar.blockAlign)
		}
		return err
	}

	capSamples := ar.samplesPerBlock * ar.numChans
	if cap(ar.pending) < capSamples {
		ar.pending = make([]int, capSamples)
	}
	out := ar.pending[:capSamples]

	if ar.numChans == 1 {
		_, err = ar.decodeMonoBlock(ar.blockBuf, out)
	} else {
		_, err = ar.decodeStereoBlock(ar.blockBuf, out)
	}
	if err != nil {
		return err
	}

	written := ar.samplesPerBlock
	// The final block can carry fewer valid samples than its capacity. The
	// fact chunk tells us how many samples actually belong to the stream.
	if ar.factSamples > 0 {
		remaining := int(ar.factSamples) - ar.framesEmitted
		if remaining <= 0 {
			written = 0
		} else if remaining < ar.samplesPerBlock {
			written = remaining
		}
	}
	ar.pending = out[:written*ar.numChans]
	ar.framesEmitted += written
	return nil
}

// Read fills dst with decoded interleaved 16 bit samples. It returns the
// number of samples written and io.EOF once the stream is exhausted.
func (ar *adpcmReader) Read(dst []int) (int, error) {
	total := 0
	for total < len(dst) {
		if len(ar.pending) == 0 {
			if ar.done {
				if err := ar.checkFact(); err != nil {
					return total, err
				}
				return total, io.EOF
			}
			if err := ar.readBlock(); err != nil {
				if errors.Is(err, io.EOF) {
					ar.done = true
					if ferr := ar.checkFact(); ferr != nil {
						return total, ferr
					}
				}
				return total, err
			}
		}
		c := copy(dst[total:], ar.pending)
		ar.pending = ar.pending[c:]
		total += c
	}
	return total, nil
}

// checkFact verifies the per-channel sample count against the fact chunk
// once all blocks have been decoded. A missing fact chunk (value 0) is
// treated as "not advertised" and skipped.
func (ar *adpcmReader) checkFact() error {
	if ar.factSamples > 0 && ar.framesEmitted != int(ar.factSamples) {
		return fmt.Errorf("%w: fact reports %d samples per channel, decoded %d",
			ErrFactMismatch, ar.factSamples, ar.framesEmitted)
	}
	return nil
}
