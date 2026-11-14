package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/riff"
)

var (
	// CIDList is the chunk ID for a LIST chunk
	CIDList = [4]byte{'L', 'I', 'S', 'T'}
	// CIDSmpl is the chunk ID for a smpl chunk
	CIDSmpl = [4]byte{'s', 'm', 'p', 'l'}
	// CIDINFO is the chunk ID for an INFO chunk
	CIDInfo = []byte{'I', 'N', 'F', 'O'}
	// CIDCue is the chunk ID for the cue chunk
	CIDCue = [4]byte{'c', 'u', 'e', 0x20}
)

// Decoder handles the decoding of wav files.
type Decoder struct {
	r      io.ReadSeeker
	parser *riff.Parser

	NumChans   uint16
	BitDepth   uint16
	SampleRate uint32

	AvgBytesPerSec uint32
	WavAudioFormat uint16

	err             error
	PCMSize         int
	pcmDataAccessed bool
	// pcmChunk is available so we can use the LimitReader
	PCMChunk *riff.Chunk
	// Metadata for the current file
	Metadata *Metadata
}

// NewDecoder creates a decoder for the passed wav reader.
// Note that the reader doesn't get rewinded as the container is processed.
func NewDecoder(r io.ReadSeeker) *Decoder {
	return &Decoder{
		r:      r,
		parser: riff.New(r),
	}
}

// Seek provides access to the cursor position in the PCM data
func (d *Decoder) Seek(offset int64, whence int) (int64, error) {
	return d.r.Seek(offset, whence)
}

// Rewind allows the decoder to be rewound to the beginning of the PCM data.
// This is useful if you want to keep on decoding the same file in a loop.
func (d *Decoder) Rewind() error {

	_, err := d.r.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("failed to seek back to the start %w", err)
	}
	// we have to user a new parser since it's read only and can't be seeked
	d.parser = riff.New(d.r)
	d.pcmDataAccessed = false
	d.PCMChunk = nil
	d.err = nil
	d.NumChans = 0
	err = d.FwdToPCM()
	if err != nil {
		return fmt.Errorf("failed to seek to the PCM data: %w", err)
	}
	return nil
}

// SampleBitDepth returns the bit depth encoding of each sample.
func (d *Decoder) SampleBitDepth() int32 {
	if d == nil {
		return 0
	}
	return int32(d.BitDepth)
}

// PCMLen returns the total number of bytes in the PCM data chunk
func (d *Decoder) PCMLen() int64 {
	if d == nil {
		return 0
	}
	return int64(d.PCMSize)
}

// isEOF reports whether err is the end of file, whether it is the bare
// io.EOF sentinel or an error wrapping it (for instance a network buffered
// reader returning fmt.Errorf("...: %w", io.EOF)).
func isEOF(err error) bool {
	return errors.Is(err, io.EOF)
}

// readFull behaves like io.ReadFull but unifies the end-of-file semantics.
// A short read caused by an end of file (bare io.EOF or a wrapped one) is
// reported as io.ErrUnexpectedEOF so truncated containers are never silently
// swallowed. A read that fills the whole buffer, even when it also carries an
// EOF, is a complete read and returns nil. Genuine, non-EOF I/O errors are
// returned untouched.
func readFull(r io.Reader, buf []byte) (int, error) {
	n, err := io.ReadFull(r, buf)
	if err != nil && isEOF(err) {
		if n < len(buf) {
			return n, io.ErrUnexpectedEOF
		}
		return n, nil
	}
	return n, err
}

// drainChunk consumes and discards the remainder of a chunk. Unlike
// riff.Chunk.Drain it reports failures: an early end of file (bare or
// wrapped) becomes io.ErrUnexpectedEOF because a short chunk is truncated
// data, and any other error is returned as-is.
func drainChunk(ch *riff.Chunk) error {
	if ch == nil || ch.R == nil {
		return nil
	}
	remaining := int64(ch.Size - ch.Pos)
	if remaining <= 0 {
		return nil
	}
	if _, err := io.CopyN(ioutil.Discard, ch.R, remaining); err != nil {
		if isEOF(err) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

// Err returns the first non-EOF error that was encountered by the Decoder.
// A normal end of file, including an io.EOF wrapped by the underlying
// reader, is not considered an error.
func (d *Decoder) Err() error {
	if d.err == nil || isEOF(d.err) {
		return nil
	}
	return d.err
}

// EOF returns positively if the underlying reader reached the end of file.
// Both a bare io.EOF and a wrapped io.EOF are treated as a normal end.
func (d *Decoder) EOF() bool {
	if d == nil {
		return true
	}
	return isEOF(d.err)
}

// IsValidFile verifies that the file is valid/readable.
func (d *Decoder) IsValidFile() bool {
	d.err = d.readHeaders()
	if d.err != nil {
		return false
	}
	if d.NumChans < 1 {
		return false
	}
	if d.BitDepth < 8 {
		return false
	}
	if d, err := d.Duration(); err != nil || d <= 0 {
		return false
	}

	return true
}

// ReadInfo reads the underlying reader until the comm header is parsed.
// This method is safe to call multiple times.
func (d *Decoder) ReadInfo() {
	d.err = d.readHeaders()
}

// ReadMetadata parses the file for extra metadata such as the INFO list chunk.
// The entire file will be read and should be rewinded if more data must be
// accessed.
func (d *Decoder) ReadMetadata() {
	if d.Metadata != nil {
		return
	}
	d.ReadInfo()
	if d.Err() != nil || d.Metadata != nil {
		return
	}
	var (
		chunk *riff.Chunk
		err   error
	)
	// terminated is set once a genuine (non-EOF) error is recorded; it must
	// not be overwritten by the normal end-of-file reached on the next read.
	terminated := false
	for {
		chunk, err = d.parser.NextChunk()
		if err != nil {
			if isEOF(err) {
				if !terminated {
					// No more chunks: the container was fully consumed.
					d.err = io.EOF
				}
			} else {
				d.err = err
			}
			break
		}

		switch chunk.ID {
		case CIDList:
			if err = DecodeListChunk(d, chunk); err != nil && !isEOF(err) {
				d.err = err
				terminated = true
			}
			if d.Metadata != nil && d.Metadata.SamplerInfo != nil {
				// we got everything we were looking for
				break
			}
		case CIDSmpl:
			if err = DecodeSamplerChunk(d, chunk); err != nil && !isEOF(err) {
				d.err = err
				terminated = true
			}
		case CIDCue:
			if err = DecodeCueChunk(d, chunk); err != nil && !isEOF(err) {
				d.err = err
				terminated = true
			}
		default:
			// fmt.Println(string(chunk.ID[:]))
			if derr := drainChunk(chunk); derr != nil {
				d.err = derr
				terminated = true
			}
		}
	}

}

// FwdToPCM forwards the underlying reader until the start of the PCM chunk.
// If the PCM chunk was already read, no data will be found (you need to rewind).
func (d *Decoder) FwdToPCM() error {
	if d == nil {
		return fmt.Errorf("PCM data not found")
	}
	d.err = d.readHeaders()
	if d.err != nil {
		return nil
	}

	var chunk *riff.Chunk
	for d.err == nil {
		chunk, d.err = d.NextChunk()
		if d.err != nil {
			if isEOF(d.err) {
				// Reaching the end of the container without a data chunk
				// means the stream is truncated or not a valid wav file.
				d.err = io.ErrUnexpectedEOF
			}
			return d.err
		}
		if chunk.ID == riff.DataFormatID {
			d.PCMSize = chunk.Size
			d.PCMChunk = chunk
			break
		}
		if chunk.ID == CIDList {
			if err := DecodeListChunk(d, chunk); err != nil && !isEOF(err) {
				d.err = err
				return d.err
			}
		}
		if err := drainChunk(chunk); err != nil {
			d.err = err
			return d.err
		}
	}
	if chunk == nil {
		return fmt.Errorf("PCM data not found")
	}
	d.pcmDataAccessed = true

	return nil
}

// WasPCMAccessed returns positively if the PCM data was previously accessed.
func (d *Decoder) WasPCMAccessed() bool {
	if d == nil {
		return false
	}
	return d.pcmDataAccessed
}

// FullPCMBuffer is an inefficient way to access all the PCM data contained in the
// audio container. The entire PCM data is held in memory.
// Consider using PCMBuffer() instead.
func (d *Decoder) FullPCMBuffer() (*audio.IntBuffer, error) {
	if !d.WasPCMAccessed() {
		err := d.FwdToPCM()
		if err != nil {
			return nil, d.err
		}
	}
	if d.PCMChunk == nil {
		return nil, errors.New("PCM chunk not found")
	}
	format := &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}

	buf := &audio.IntBuffer{Data: make([]int, 4096), Format: format, SourceBitDepth: int(d.BitDepth)}
	bytesPerSample := (d.BitDepth-1)/8 + 1
	sampleBufData := make([]byte, bytesPerSample)
	if _, err := decodeSampleBytes(int(d.BitDepth), sampleBufData); err != nil {
		return nil, fmt.Errorf("could not get sample decoder %w", err)
	}

	i := 0
	var err error
	for {
		// Read a whole sample through the chunk so its read position is
		// tracked. The position against PCMSize (the unpadded PCM length)
		// distinguishes a clean end from truncation regardless of whether
		// the underlying reader returns a bare or a wrapped io.EOF, alone or
		// together with the final bytes.
		n, rerr := io.ReadFull(d.PCMChunk, sampleBufData)
		if rerr != nil {
			if isEOF(rerr) {
				if d.PCMChunk.Pos >= d.PCMSize {
					// Normal end of file; any fully read sample above was
					// already counted.
					d.err = io.EOF
					err = nil
				} else {
					// The stream ended before the declared PCM data.
					d.err = io.ErrUnexpectedEOF
					err = io.ErrUnexpectedEOF
				}
			} else {
				// A genuine I/O failure stays fully traceable.
				d.err = rerr
				err = rerr
			}
			break
		}
		if n < len(sampleBufData) {
			d.err = io.ErrUnexpectedEOF
			err = io.ErrUnexpectedEOF
			break
		}
		buf.Data[i], err = decodeSampleBytes(int(d.BitDepth), sampleBufData)
		if err != nil {
			d.err = err
			break
		}
		i++
		// grow the underlying slice if needed
		if i == len(buf.Data) {
			buf.Data = append(buf.Data, make([]int, 4096)...)
		}
	}
	buf.Data = buf.Data[:i]

	return buf, err
}

// PCMBuffer populates the passed PCM buffer
func (d *Decoder) PCMBuffer(buf *audio.IntBuffer) (n int, err error) {
	if buf == nil {
		return 0, nil
	}

	if !d.pcmDataAccessed {
		err := d.FwdToPCM()
		if err != nil {
			return 0, d.err
		}
	}
	if d.PCMChunk == nil {
		return 0, ErrPCMChunkNotFound
	}

	format := &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}

	buf.SourceBitDepth = int(d.BitDepth)
	decodeF, err := sampleDecodeFunc(int(d.BitDepth))
	if err != nil {
		return 0, fmt.Errorf("could not get sample decode func %w", err)
	}

	bPerSample := bytesPerSample(int(d.BitDepth))
	// populate a file buffer to avoid multiple very small reads
	// we need to cap the buffer size to not be bigger than the pcm chunk.
	size := len(buf.Data) * bPerSample
	tmpBuf := make([]byte, size)
	var m int
	// Read through the chunk so its read position is tracked; this lets us
	// tell a clean end of file from a truncated stream even when the
	// underlying reader wraps io.EOF.
	m, err = d.PCMChunk.Read(tmpBuf)
	if err != nil {
		if isEOF(err) {
			// PCMSize is the unpadded size of the PCM data; reaching it is
			// a clean end, stopping earlier means the stream was truncated.
			if d.PCMChunk.Pos < d.PCMSize {
				d.err = io.ErrUnexpectedEOF
				return m, io.ErrUnexpectedEOF
			}
			// A normal end of file: the returned bytes (if any) are still
			// decoded below.
			d.err = io.EOF
			err = nil
		} else {
			d.err = err
			return m, err
		}
	}
	if m == 0 {
		return m, nil
	}
	bufR := bytes.NewReader(tmpBuf[:m])
	sampleBuf := make([]byte, bPerSample, bPerSample)
	var misaligned bool
	if m%bPerSample > 0 {
		misaligned = true
	}

	// Note that we populate the buffer even if the
	// size of the buffer doesn't fit an even number of frames.
	for n = 0; n < len(buf.Data); n++ {
		buf.Data[n], err = decodeF(bufR, sampleBuf)
		if err != nil {
			// the last sample isn't a full sample but just padding.
			if misaligned {
				n--
			}
			break
		}
	}
	buf.Format = format
	if isEOF(err) {
		err = nil
	}

	return n, err
}

// Format returns the audio format of the decoded content.
func (d *Decoder) Format() *audio.Format {
	if d == nil {
		return nil
	}
	return &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}
}

// NextChunk returns the next available chunk
func (d *Decoder) NextChunk() (*riff.Chunk, error) {
	if d.err = d.readHeaders(); d.err != nil {
		d.err = fmt.Errorf("failed to read header: %w", d.err)
		return nil, d.err
	}

	var (
		id   [4]byte
		size uint32
	)

	id, size, d.err = d.parser.IDnSize()
	if d.err != nil {
		d.err = fmt.Errorf("error reading chunk header: %w", d.err)
		return nil, d.err
	}

	// TODO: any reason we don't use d.parser.NextChunk (riff.NextChunk) here?
	// It correctly handles the misaligned chunk.

	// TODO: copied over from riff.parser.NextChunk
	// all RIFF chunks (including WAVE "data" chunks) must be word aligned.
	// If the data uses an odd number of bytes, a padding byte with a value of zero must be placed at the end of the sample data.
	// The "data" chunk header's size should not include this byte.
	if size%2 == 1 {
		size++
	}

	c := &riff.Chunk{
		ID:   id,
		Size: int(size),
		R:    io.LimitReader(d.r, int64(size)),
	}
	return c, d.err
}

// Duration returns the time duration for the current audio container
func (d *Decoder) Duration() (time.Duration, error) {
	if d == nil || d.parser == nil {
		return 0, errors.New("can't calculate the duration of a nil pointer")
	}
	return d.parser.Duration()
}

// String implements the Stringer interface.
func (d *Decoder) String() string {
	return d.parser.String()
}

// readHeaders is safe to call multiple times
func (d *Decoder) readHeaders() error {
	if d == nil || d.NumChans > 0 {
		return nil
	}

	id, size, err := d.parser.IDnSize()
	if err != nil {
		// An empty or truncated container has no RIFF header.
		if isEOF(err) {
			err = io.ErrUnexpectedEOF
		}
		d.err = err
		return err
	}
	d.parser.ID = id
	if d.parser.ID != riff.RiffID {
		d.err = fmt.Errorf("%s - %s", d.parser.ID, riff.ErrFmtNotSupported)
		return d.err
	}
	d.parser.Size = size
	if err := binary.Read(d.r, binary.BigEndian, &d.parser.Format); err != nil {
		if isEOF(err) {
			err = io.ErrUnexpectedEOF
		}
		d.err = err
		return err
	}

	var chunk *riff.Chunk
	var rewindBytes int64

	for {
		chunk, err = d.parser.NextChunk()
		if err != nil {
			// The fmt chunk was never found: the header is truncated.
			if isEOF(err) {
				err = io.ErrUnexpectedEOF
			}
			d.err = err
			return err
		}

		if chunk.ID == riff.FmtID {
			chunk.DecodeWavHeader(d.parser)
			d.NumChans = d.parser.NumChannels
			d.BitDepth = d.parser.BitsPerSample
			d.SampleRate = d.parser.SampleRate
			d.WavAudioFormat = d.parser.WavAudioFormat
			d.AvgBytesPerSec = d.parser.AvgBytesPerSec

			if rewindBytes > 0 {
				if _, err := d.r.Seek(-(rewindBytes + int64(chunk.Size) + 8), io.SeekCurrent); err != nil {
					d.err = err
					return err
				}
			}
			break
		} else if chunk.ID == CIDList {
			// The list chunk can be in the header or footer
			// because so many players don't support that chunk properly
			// it is recommended to have it at the end of the file.
			if err = DecodeListChunk(d, chunk); err != nil && !isEOF(err) {
				d.err = err
				return err
			}
			// unexpected chunk order, might be a bext chunk
			rewindBytes += int64(chunk.Size) + 8
		} else if chunk.ID == CIDSmpl {
			if err = DecodeSamplerChunk(d, chunk); err != nil && !isEOF(err) {
				d.err = err
				return err
			}
			rewindBytes += int64(chunk.Size) + 8
		} else {
			// unexpected chunk order, might be a bext chunk
			rewindBytes += int64(chunk.Size) + 8
			// drain the chunk
			if _, err := io.CopyN(ioutil.Discard, d.r, int64(chunk.Size)); err != nil {
				if isEOF(err) {
					err = io.ErrUnexpectedEOF
				}
				d.err = err
				return err
			}
		}
	}

	return nil
}

func bytesPerSample(bitDepth int) int {
	return bitDepth / 8
}

// decodeSampleBytes converts a full, little-endian encoded sample into an
// int based on the bit depth. The passed buffer must hold a whole sample.
// Note that 8bit samples are unsigned, all other values are signed.
func decodeSampleBytes(bitsPerSample int, buf []byte) (int, error) {
	switch bitsPerSample {
	case 8:
		// 8bit values are unsigned
		return int(buf[0]), nil
	case 16:
		return int(int16(binary.LittleEndian.Uint16(buf[:2]))), nil
	case 24:
		return int(audio.Int24LETo32(buf[:3])), nil
	case 32:
		return int(int32(binary.LittleEndian.Uint32(buf[:4]))), nil
	default:
		return 0, fmt.Errorf("unhandled byte depth:%d", bitsPerSample)
	}
}

// sampleDecodeFunc returns a function that can be used to convert
// a byte range into an int value based on the amount of bits used per sample.
// Note that 8bit samples are unsigned, all other values are signed.
func sampleDecodeFunc(bitsPerSample int) (func(io.Reader, []byte) (int, error), error) {
	// NOTE: WAV PCM data is stored using little-endian
	switch bitsPerSample {
	case 8:
		// 8bit values are unsigned
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:1])
			return int(buf[0]), err
		}, nil
	case 16:
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:2])
			return int(int16(binary.LittleEndian.Uint16(buf[:2]))), err
		}, nil
	case 24:
		// -34,359,738,367 (0x7FFFFF) to 34,359,738,368	(0x800000)
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:3])
			if err != nil {
				return 0, err
			}
			return int(audio.Int24LETo32(buf[:3])), nil
		}, nil
	case 32:
		return func(r io.Reader, buf []byte) (int, error) {
			_, err := r.Read(buf[:4])
			return int(int32(binary.LittleEndian.Uint32(buf[:4]))), err
		}, nil
	default:
		return nil, fmt.Errorf("unhandled byte depth:%d", bitsPerSample)
	}
}

// sampleDecodeFloat64Func returns a function that can be used to convert
// a byte range into a float64 value based on the amount of bits used per sample.
func sampleFloat64DecodeFunc(bitsPerSample int) (func([]byte) float64, error) {
	bytesPerSample := bitsPerSample / 8
	switch bytesPerSample {
	case 1:
		// 8bit values are unsigned
		return func(s []byte) float64 {
			return float64(uint8(s[0]))
		}, nil
	case 2:
		return func(s []byte) float64 {
			return float64(int(s[0]) + int(s[1])<<8)
		}, nil
	case 3:
		return func(s []byte) float64 {
			var output int32
			output |= int32(s[2]) << 0
			output |= int32(s[1]) << 8
			output |= int32(s[0]) << 16
			return float64(output)
		}, nil
	case 4:
		// TODO: fix the float64 conversion (current int implementation)
		return func(s []byte) float64 {
			return float64(int(s[0]) + int(s[1])<<8 + int(s[2])<<16 + int(s[3])<<24)
		}, nil
	default:
		return nil, fmt.Errorf("unhandled byte depth:%d", bitsPerSample)
	}
}
