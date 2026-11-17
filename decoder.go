package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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

// CIDFact is the chunk ID for the fact chunk, which carries the number of
// samples per channel of non-PCM (e.g. IMA ADPCM) data.
var CIDFact = [4]byte{'f', 'a', 'c', 't'}

// Decoder handles the decoding of wav files.
type Decoder struct {
	r      io.ReadSeeker
	parser *riff.Parser

	NumChans   uint16
	BitDepth   uint16
	SampleRate uint32

	AvgBytesPerSec uint32
	WavAudioFormat uint16
	// BlockAlign is the number of bytes per block of audio data.
	BlockAlign uint16
	// FactSamples is the per-channel sample count stored in the fact chunk.
	// It is only populated for non-PCM formats that include a fact chunk.
	FactSamples uint32
	// ADPCMSamplesPerBlock is the per-channel sample count advertised by the
	// IMA ADPCM fmt extension (cbSize + wSamplesPerBlock).
	ADPCMSamplesPerBlock uint16

	err     error
	PCMSize int
	// pcmDataSize is the data chunk size as declared in its header, before
	// RIFF word alignment padding is applied.
	pcmDataSize     int
	pcmDataAccessed bool
	// pcmChunk is available so we can use the LimitReader
	PCMChunk *riff.Chunk
	// Metadata for the current file
	Metadata *Metadata

	// adpcm is lazily initialized when IMA ADPCM data is decoded.
	adpcm *adpcmReader
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
	d.adpcm = nil
	d.FactSamples = 0
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

// Err returns the first non-EOF error that was encountered by the Decoder.
func (d *Decoder) Err() error {
	if errors.Is(d.err, io.EOF) {
		return nil
	}
	return d.err
}

// EOF returns positively if the underlying reader reached the end of file.
func (d *Decoder) EOF() bool {
	if d == nil || errors.Is(d.err, io.EOF) {
		return true
	}
	return false
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
	minBitDepth := 8
	if d.WavAudioFormat == WavFormatIMAADPCM {
		minBitDepth = 4
	}
	if d.BitDepth < uint16(minBitDepth) {
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
	for err == nil {
		chunk, err = d.parser.NextChunk()
		if err != nil {
			break
		}

		switch chunk.ID {
		case CIDList:
			if err = DecodeListChunk(d, chunk); err != nil {
				if !errors.Is(err, io.EOF) {
					d.err = err
				}
			}
			if d.Metadata != nil && d.Metadata.SamplerInfo != nil {
				// we got everything we were looking for
				break
			}
		case CIDSmpl:
			if err = DecodeSamplerChunk(d, chunk); err != nil {
				if !errors.Is(err, io.EOF) {
					d.err = err
				}
			}
		case CIDCue:
			if err = DecodeCueChunk(d, chunk); err != nil {
				if !errors.Is(err, io.EOF) {
					d.err = err
				}
			}
		default:
			// fmt.Println(string(chunk.ID[:]))
			chunk.Drain()
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
		return d.err
	}

	var chunk *riff.Chunk
	for d.err == nil {
		chunk, d.err = d.NextChunk()
		if d.err != nil {
			return d.err
		}
		if chunk.ID == riff.DataFormatID {
			d.PCMSize = chunk.Size
			d.PCMChunk = chunk
			break
		}
		if chunk.ID == CIDFact {
			if err := d.decodeFactChunk(chunk); err != nil {
				return err
			}
		}
		if chunk.ID == CIDList {
			DecodeListChunk(d, chunk)
		}
		chunk.Drain()
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

	if d.WavAudioFormat == WavFormatIMAADPCM {
		return d.fullADPCMBuffer(format)
	}

	buf := &audio.IntBuffer{Data: make([]int, 4096), Format: format, SourceBitDepth: int(d.BitDepth)}
	bytesPerSample := (d.BitDepth-1)/8 + 1
	sampleBufData := make([]byte, bytesPerSample)
	decodeF, err := sampleDecodeFunc(int(d.BitDepth))
	if err != nil {
		return nil, fmt.Errorf("could not get sample decode func %w", err)
	}

	i := 0
	for err == nil {
		buf.Data[i], err = decodeF(d.PCMChunk, sampleBufData)
		if err != nil {
			break
		}
		i++
		// grow the underlying slice if needed
		if i == len(buf.Data) {
			buf.Data = append(buf.Data, make([]int, 4096)...)
		}
	}
	buf.Data = buf.Data[:i]

	if errors.Is(err, io.EOF) {
		err = nil
	}

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

	if d.WavAudioFormat == WavFormatIMAADPCM {
		return d.pcmADPCMBuffer(buf)
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
	m, err = d.PCMChunk.R.Read(tmpBuf)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return m, nil
		}
		return m, err
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
	if errors.Is(err, io.EOF) {
		err = nil
	}

	return n, err
}

// initADPCMReader validates the IMA ADPCM fmt parameters and creates the
// block decoder reading from the data chunk. It is safe to call once per
// decoder instance; rewinding resets the decoder.
func (d *Decoder) initADPCMReader() error {
	if d.adpcm != nil {
		return nil
	}
	// Only consume the bytes declared by the data chunk header. A possible
	// RIFF word alignment padding byte must not be mistaken for a block.
	dataReader := io.LimitReader(d.PCMChunk.R, int64(d.pcmDataSize))
	ar, err := newADPCMReader(dataReader, int(d.NumChans), int(d.BlockAlign), d.FactSamples)
	if err != nil {
		return err
	}
	// Cross-check the samples per block advertised by the fmt extension
	// against the value derived from the block align.
	if d.ADPCMSamplesPerBlock != 0 &&
		int(d.ADPCMSamplesPerBlock) != ar.samplesPerBlock {
		return fmt.Errorf("%w: fmt extension advertises %d samples per block but block align %d yields %d",
			ErrInvalidBlockAlign, d.ADPCMSamplesPerBlock, d.BlockAlign, ar.samplesPerBlock)
	}
	d.adpcm = ar
	return nil
}

// pcmADPCMBuffer decodes IMA ADPCM blocks into the passed buffer as
// interleaved signed 16 bit PCM samples.
func (d *Decoder) pcmADPCMBuffer(buf *audio.IntBuffer) (int, error) {
	if buf == nil {
		return 0, nil
	}
	if err := d.initADPCMReader(); err != nil {
		return 0, err
	}
	buf.SourceBitDepth = 16
	buf.Format = &audio.Format{
		NumChannels: int(d.NumChans),
		SampleRate:  int(d.SampleRate),
	}
	n, err := d.adpcm.Read(buf.Data)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

// fullADPCMBuffer decodes the entire IMA ADPCM data chunk into one buffer.
func (d *Decoder) fullADPCMBuffer(format *audio.Format) (*audio.IntBuffer, error) {
	if err := d.initADPCMReader(); err != nil {
		return nil, err
	}
	buf := &audio.IntBuffer{
		Data:           make([]int, 0, 4096),
		Format:         format,
		SourceBitDepth: 16,
	}
	tmp := make([]int, 4096)
	for {
		n, err := d.adpcm.Read(tmp)
		buf.Data = append(buf.Data, tmp[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return buf, err
		}
	}
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
		d.err = fmt.Errorf("failed to read header - %v", d.err)
		return nil, d.err
	}

	var (
		id   [4]byte
		size uint32
	)

	id, size, d.err = d.parser.IDnSize()
	if d.err != nil {
		d.err = fmt.Errorf("error reading chunk header - %v", d.err)
		return nil, d.err
	}
	rawSize := size

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
	// Remember the unpadded size so compressed block decoders can ignore
	// the RIFF padding byte of an odd sized data chunk.
	d.pcmDataSize = int(rawSize)
	return c, d.err
}

// Duration returns the time duration for the current audio container
func (d *Decoder) Duration() (time.Duration, error) {
	if d == nil || d.parser == nil {
		return 0, errors.New("can't calculate the duration of a nil pointer")
	}
	if d.WavAudioFormat == WavFormatIMAADPCM && d.FactSamples > 0 && d.SampleRate > 0 {
		return time.Duration(float64(d.FactSamples) / float64(d.SampleRate) * float64(time.Second)), nil
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
		return err
	}
	d.parser.ID = id
	if d.parser.ID != riff.RiffID {
		return fmt.Errorf("%s - %s", d.parser.ID, riff.ErrFmtNotSupported)
	}
	d.parser.Size = size
	if err := binary.Read(d.r, binary.BigEndian, &d.parser.Format); err != nil {
		return err
	}

	var chunk *riff.Chunk
	var rewindBytes int64

	for err == nil {
		chunk, err = d.parser.NextChunk()
		if err != nil {
			break
		}

		if chunk.ID == riff.FmtID {
			if err := d.decodeFmtChunk(chunk); err != nil {
				d.err = err
				return err
			}

			if rewindBytes > 0 {
				d.r.Seek(-(rewindBytes + int64(chunk.Size) + 8), 1)
			}
			break
		} else if chunk.ID == CIDFact {
			if err := d.decodeFactChunk(chunk); err != nil {
				d.err = err
				return err
			}
			rewindBytes += int64(chunk.Size) + 8
		} else if chunk.ID == CIDList {
			// The list chunk can be in the header or footer
			// because so many players don't support that chunk properly
			// it is recommended to have it at the end of the file.
			DecodeListChunk(d, chunk)
			// unexpected chunk order, might be a bext chunk
			rewindBytes += int64(chunk.Size) + 8
		} else if chunk.ID == CIDSmpl {
			DecodeSamplerChunk(d, chunk)
			rewindBytes += int64(chunk.Size) + 8
		} else {
			// unexpected chunk order, might be a bext chunk
			rewindBytes += int64(chunk.Size) + 8
			// drain the chunk
			io.CopyN(io.Discard, d.r, int64(chunk.Size))
		}
	}

	return d.err
}

func bytesPerSample(bitDepth int) int {
	return bitDepth / 8
}

// decodeFmtChunk parses the full fmt chunk (including the format specific
// extension bytes) from the chunk reader. The third party riff parser only
// consumes the 16 standard WAVEFORMATEX bytes and drains the rest, so the
// IMA ADPCM extension (cbSize + wSamplesPerBlock) is decoded here instead.
func (d *Decoder) decodeFmtChunk(chunk *riff.Chunk) error {
	var header struct {
		AudioFormat    uint16
		NumChannels    uint16
		SampleRate     uint32
		AvgBytesPerSec uint32
		BlockAlign     uint16
		BitsPerSample  uint16
	}
	if err := chunk.ReadLE(&header); err != nil {
		return fmt.Errorf("failed to read wav fmt header: %w", err)
	}
	d.WavAudioFormat = header.AudioFormat
	d.NumChans = header.NumChannels
	d.SampleRate = header.SampleRate
	d.AvgBytesPerSec = header.AvgBytesPerSec
	d.BlockAlign = header.BlockAlign
	d.BitDepth = header.BitsPerSample
	// Keep the shared riff parser in sync for callers that use it directly
	// (e.g. Duration()).
	d.parser.WavAudioFormat = header.AudioFormat
	d.parser.NumChannels = header.NumChannels
	d.parser.SampleRate = header.SampleRate
	d.parser.AvgBytesPerSec = header.AvgBytesPerSec
	d.parser.BlockAlign = header.BlockAlign
	d.parser.BitsPerSample = header.BitsPerSample

	if d.WavAudioFormat == WavFormatIMAADPCM {
		// The extension holds a uint16 cbSize followed by cbSize bytes;
		// for IMA ADPCM the first extension field is wSamplesPerBlock.
		if chunk.Size < 18 {
			return fmt.Errorf("%w: IMA ADPCM fmt chunk must include a 2 byte extension, got %d bytes",
				ErrInvalidBlockAlign, chunk.Size)
		}
		var cbSize uint16
		if err := chunk.ReadLE(&cbSize); err != nil {
			return fmt.Errorf("failed to read IMA ADPCM cbSize: %w", err)
		}
		if cbSize < 2 {
			return fmt.Errorf("%w: IMA ADPCM cbSize must be at least 2, got %d", ErrInvalidBlockAlign, cbSize)
		}
		if err := chunk.ReadLE(&d.ADPCMSamplesPerBlock); err != nil {
			return fmt.Errorf("failed to read IMA ADPCM samples per block: %w", err)
		}
		if err := d.validateIMAFormat(); err != nil {
			return err
		}
	}
	return nil
}

// decodeFactChunk reads the per-channel sample count stored in a fact
// chunk. The chunk size excludes RIFF word alignment padding.
func (d *Decoder) decodeFactChunk(chunk *riff.Chunk) error {
	if chunk.Size < 4 {
		return fmt.Errorf("wav: fact chunk must hold at least 4 bytes, got %d", chunk.Size)
	}
	var fact [4]byte
	if _, err := io.ReadFull(chunk.R, fact[:]); err != nil {
		return fmt.Errorf("failed to read fact chunk: %w", err)
	}
	d.FactSamples = binary.LittleEndian.Uint32(fact[:])
	return nil
}

// validateIMAFormat verifies the fmt values required to decode IMA ADPCM.
func (d *Decoder) validateIMAFormat() error {
	if d.BitDepth != 4 {
		return fmt.Errorf("%w: IMA ADPCM uses 4 bit samples, fmt declares %d bits",
			ErrInvalidBlockAlign, d.BitDepth)
	}
	if d.NumChans != 1 && d.NumChans != 2 {
		return fmt.Errorf("%w: IMA ADPCM supports 1 or 2 channels, got %d",
			ErrInvalidBlockAlign, d.NumChans)
	}
	if d.BlockAlign == 0 {
		return fmt.Errorf("%w: block align must be greater than 0", ErrInvalidBlockAlign)
	}
	minAlign := uint16(4)
	if d.NumChans == 2 {
		minAlign = 8
		if (d.BlockAlign-8)%8 != 0 {
			return fmt.Errorf("%w: stereo block align %d must be 8 plus a multiple of 8",
				ErrInvalidBlockAlign, d.BlockAlign)
		}
	}
	if d.BlockAlign < minAlign {
		return fmt.Errorf("%w: block align %d is below the minimum %d bytes",
			ErrInvalidBlockAlign, d.BlockAlign, minAlign)
	}
	if d.ADPCMSamplesPerBlock != 0 &&
		int(d.ADPCMSamplesPerBlock) != samplesPerADPCMBlock(int(d.BlockAlign), int(d.NumChans)) {
		return fmt.Errorf("%w: samples per block %d does not match block align %d",
			ErrInvalidBlockAlign, d.ADPCMSamplesPerBlock, d.BlockAlign)
	}
	return nil
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
