package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/go-audio/riff"
)

// RF64 is the EBU Tech 3306 64-bit extension of the RIFF/WAVE container.
//
// An RF64 file starts with the "RF64" FourCC instead of "RIFF", the 32-bit
// RIFF size and data chunk size fields carry the 0xFFFFFFFF sentinel, and the
// real 64-bit lengths live in the mandatory "ds64" chunk (the first chunk
// after the WAVE FourCC).
var (
	// rf64ID is the root container ID of an RF64 file.
	rf64ID = [4]byte{'R', 'F', '6', '4'}
	// ds64ID is the ID of the data-size-64 chunk.
	ds64ID = [4]byte{'d', 's', '6', '4'}

	// rf64SizeSentinel is the value stored in every 32-bit size field that
	// overflowed and whose real value is held in the ds64 chunk.
	rf64SizeSentinel uint32 = math.MaxUint32

	// ds64FixedSize is the minimum ds64 chunk body size in bytes:
	// riffSize64 + dataSize64 + sampleCount64 + tableLength.
	ds64FixedSize = 28
	// ds64TableEntrySize is the size of one ds64 table entry:
	// FourCC + uint64 size.
	ds64TableEntrySize = 12
)

// Errors returned when reading or writing RF64 content. Callers can use
// errors.Is to match them.
var (
	// ErrMissingDS64 indicates an RF64 file without a ds64 chunk in the
	// mandatory first-chunk position.
	ErrMissingDS64 = errors.New("wav: RF64 file is missing its ds64 chunk")
	// ErrTruncatedDS64 indicates a ds64 chunk whose body is shorter than
	// what its header promises.
	ErrTruncatedDS64 = errors.New("wav: ds64 chunk is truncated")
	// ErrDS64Contradiction indicates lengths declared in the ds64 chunk do
	// not agree with each other, with the 32-bit chunk headers or with the
	// physical size of the file.
	ErrDS64Contradiction = errors.New("wav: ds64 chunk contains contradictory lengths")
	// ErrRF64BadSentinel indicates an RF64 32-bit field that does not carry
	// the required 0xFFFFFFFF sentinel.
	ErrRF64BadSentinel = errors.New("wav: invalid RF64 sentinel value")
	// ErrRF64MissingSize indicates a chunk using the 0xFFFFFFFF sentinel
	// while the ds64 chunk provides no replacement size for it.
	ErrRF64MissingSize = errors.New("wav: RF64 chunk uses the sentinel size but ds64 has no entry for it")
	// ErrRIFFSizeOverflow indicates a classic RIFF file grew past the
	// 32-bit container size limit. The encoder has to be switched to RF64.
	ErrRIFFSizeOverflow = errors.New("wav: RIFF container size exceeds the 32-bit limit, enable RF64")
	// ErrPCMSizeOverflow indicates a WAVE data chunk grew past the 32-bit
	// chunk size limit. The encoder has to be switched to RF64.
	ErrPCMSizeOverflow = errors.New("wav: WAVE data chunk size exceeds the 32-bit limit, enable RF64")
	// ErrRF64RequiresSeek indicates RF64 output was requested on a writer
	// that cannot seek, so the 64-bit sizes cannot be backfilled on Close.
	ErrRF64RequiresSeek = errors.New("wav: RF64 requires an io.WriteSeeker so the 64-bit sizes can be backfilled")
)

// ds64Info holds the parsed content of the ds64 chunk.
type ds64Info struct {
	// riffSize64 is the equivalent of the RIFF 32-bit size: the total file
	// size minus 8 bytes.
	riffSize64 uint64
	// dataSize64 is the real size of the data chunk payload in bytes.
	dataSize64 uint64
	// sampleCount64 is the number of sample frames (samples per channel).
	sampleCount64 uint64
	// chunkSizes stores the 64-bit sizes of non-data chunks whose 32-bit
	// size field overflows, keyed by chunk FourCC.
	chunkSizes map[[4]byte]uint64
}

// IsRF64 reports whether the decoder is reading an RF64 (64-bit WAVE) file.
func (d *Decoder) IsRF64() bool {
	if d == nil {
		return false
	}
	return d.isRF64
}

// writeAtLE seeks to pos and writes v little-endian without touching the
// encoder's sequential WrittenBytes counter.
func (e *Encoder) writeAtLE(pos int64, v interface{}) error {
	if _, err := e.w.Seek(pos, io.SeekStart); err != nil {
		return fmt.Errorf("%w: %v", ErrRF64RequiresSeek, err)
	}
	if err := binary.Write(e.w, binary.LittleEndian, v); err != nil {
		return err
	}
	return nil
}

// writeRF64Header writes the RF64 root header and the mandatory ds64 chunk
// (with placeholder 64-bit values backfilled on Close) followed by the
// regular fmt chunk.
func (e *Encoder) writeRF64Header() error {
	// Fail fast on backends that advertise io.WriteSeeker but cannot seek:
	// the 64-bit values have to be backfilled in place on Close.
	if _, err := e.w.Seek(0, io.SeekCurrent); err != nil {
		return fmt.Errorf("%w: %v", ErrRF64RequiresSeek, err)
	}

	// "RF64" root FourCC, the 32-bit size stays at the sentinel forever.
	if err := e.AddLE(rf64ID); err != nil {
		return err
	}
	if err := e.AddLE(rf64SizeSentinel); err != nil {
		return err
	}
	if err := e.AddLE(riff.WavFormatID); err != nil {
		return err
	}

	// ds64 chunk, mandatory as the first chunk of an RF64 file.
	if err := e.AddLE(ds64ID); err != nil {
		return err
	}
	if err := e.AddLE(uint32(ds64FixedSize)); err != nil {
		return err
	}
	// Remember every position so Close can backfill the real values.
	e.ds64RiffSizePos = int64(e.WrittenBytes)
	if err := e.AddLE(uint64(math.MaxUint64)); err != nil { // riffSize64
		return err
	}
	e.ds64DataSizePos = int64(e.WrittenBytes)
	if err := e.AddLE(uint64(math.MaxUint64)); err != nil { // dataSize64
		return err
	}
	e.ds64SampleCountPos = int64(e.WrittenBytes)
	if err := e.AddLE(uint64(0)); err != nil { // sampleCount64
		return err
	}
	if err := e.AddLE(uint32(0)); err != nil { // table length, no extra entries
		return err
	}

	// Regular fmt chunk, identical to the RIFF layout.
	if err := e.AddLE(riff.FmtID); err != nil {
		return err
	}
	if err := e.AddLE(uint32(16)); err != nil {
		return err
	}
	if err := e.AddLE(uint16(e.WavAudioFormat)); err != nil {
		return err
	}
	if err := e.AddLE(uint16(e.NumChans)); err != nil {
		return fmt.Errorf("error encoding the number of channels - %w", err)
	}
	if err := e.AddLE(uint32(e.SampleRate)); err != nil {
		return fmt.Errorf("error encoding the sample rate - %w", err)
	}
	blockAlign := e.NumChans * e.BitDepth / 8
	if err := e.AddLE(uint32(e.SampleRate * blockAlign)); err != nil {
		return fmt.Errorf("error encoding the avg bytes per sec - %w", err)
	}
	if err := e.AddLE(uint16(blockAlign)); err != nil {
		return err
	}
	if err := e.AddLE(uint16(e.BitDepth)); err != nil {
		return fmt.Errorf("error encoding bits per sample - %w", err)
	}

	return nil
}

// closeRF64 backfills the ds64 chunk and the data chunk 32-bit size once the
// total amount of PCM data is known.
func (e *Encoder) closeRF64() error {
	dataSize := uint64(e.BitDepth/8) * uint64(e.NumChans) * uint64(e.frames)
	// WrittenBytes counts every byte of the container, the RIFF size field
	// excludes the 8-byte root header.
	riffSize := uint64(e.WrittenBytes) - 8

	if err := e.writeAtLE(e.ds64RiffSizePos, riffSize); err != nil {
		return fmt.Errorf("failed to backfill the RF64 RIFF size: %w", err)
	}
	if err := e.writeAtLE(e.ds64DataSizePos, dataSize); err != nil {
		return fmt.Errorf("failed to backfill the RF64 data size: %w", err)
	}
	if err := e.writeAtLE(e.ds64SampleCountPos, uint64(e.frames)); err != nil {
		return fmt.Errorf("failed to backfill the RF64 sample count: %w", err)
	}

	// The data chunk 32-bit size only uses the sentinel when it overflows,
	// small RF64 files carry the real value there.
	dataSize32 := uint32(dataSize)
	if dataSize > math.MaxUint32 {
		dataSize32 = rf64SizeSentinel
	}
	if e.pcmChunkSizePos > 0 {
		if err := e.writeAtLE(int64(e.pcmChunkSizePos), dataSize32); err != nil {
			return fmt.Errorf("failed to backfill the RF64 data chunk size: %w", err)
		}
	}

	if _, err := e.w.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("%w: %v", ErrRF64RequiresSeek, err)
	}
	return nil
}

// readDS64 parses the mandatory ds64 chunk. On entry the reader is positioned
// right after the WAVE FourCC; on success it sits just past the chunk
// (including its odd-byte padding, if any).
func (d *Decoder) readDS64() error {
	var (
		chunkID   [4]byte
		chunkSz32 uint32
	)
	if err := binary.Read(d.r, binary.BigEndian, &chunkID); err != nil {
		return fmt.Errorf("%w: no chunk follows the WAVE FourCC: %v", ErrMissingDS64, err)
	}
	if err := binary.Read(d.r, binary.LittleEndian, &chunkSz32); err != nil {
		return fmt.Errorf("%w: cannot read ds64 chunk size: %v", ErrTruncatedDS64, err)
	}
	if chunkID != ds64ID {
		return fmt.Errorf("%w: first RF64 chunk must be ds64, got %q", ErrMissingDS64, chunkID)
	}
	if chunkSz32 < uint32(ds64FixedSize) {
		return fmt.Errorf("%w: ds64 body is %d bytes, minimum is %d", ErrTruncatedDS64, chunkSz32, ds64FixedSize)
	}

	body := make([]byte, chunkSz32)
	if _, err := io.ReadFull(d.r, body); err != nil {
		return fmt.Errorf("%w: %v", ErrTruncatedDS64, err)
	}

	le := binary.LittleEndian
	info := &ds64Info{
		riffSize64:    le.Uint64(body[0:8]),
		dataSize64:    le.Uint64(body[8:16]),
		sampleCount64: le.Uint64(body[16:24]),
		chunkSizes:    make(map[[4]byte]uint64),
	}
	tableLen := le.Uint32(body[24:28])
	// The chunk size must exactly account for the fixed part and the table.
	if uint64(chunkSz32) != uint64(ds64FixedSize)+uint64(tableLen)*uint64(ds64TableEntrySize) {
		return fmt.Errorf("%w: ds64 chunk size %d does not match table length %d", ErrDS64Contradiction, chunkSz32, tableLen)
	}
	off := ds64FixedSize
	for i := uint32(0); i < tableLen; i++ {
		var cid [4]byte
		copy(cid[:], body[off:off+4])
		info.chunkSizes[cid] = le.Uint64(body[off+4 : off+12])
		off += ds64TableEntrySize
	}
	// RIFF chunks are word aligned, skip the padding byte of an odd-sized chunk.
	if chunkSz32%2 == 1 {
		var pad [1]byte
		if _, err := io.ReadFull(d.r, pad[:]); err != nil {
			return fmt.Errorf("%w: missing ds64 padding byte: %v", ErrTruncatedDS64, err)
		}
	}

	// A riffSize64 of all ones means "length unknown" on unseekable streams.
	if info.riffSize64 != math.MaxUint64 {
		if info.dataSize64 > info.riffSize64 {
			return fmt.Errorf("%w: data size %d is larger than RIFF size %d", ErrDS64Contradiction, info.dataSize64, info.riffSize64)
		}
		if err := d.verifyRF64FileSize(info.riffSize64); err != nil {
			return err
		}
	}

	d.ds64 = info
	return nil
}

// verifyRF64FileSize cross-checks the ds64 RIFF size against the physical
// size of the underlying seekable reader.
func (d *Decoder) verifyRF64FileSize(riffSize64 uint64) error {
	cur, err := d.r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil // best effort
	}
	end, err := d.r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil // best effort
	}
	if _, err := d.r.Seek(cur, io.SeekStart); err != nil {
		return err
	}
	if riffSize64 > math.MaxInt64 {
		return fmt.Errorf("%w: RF64 RIFF size %d does not fit on this platform", ErrDS64Contradiction, riffSize64)
	}
	if want := int64(riffSize64) + 8; end != want {
		return fmt.Errorf("%w: ds64 RIFF size implies a %d byte file but the reader holds %d bytes", ErrDS64Contradiction, want, end)
	}
	return nil
}

// resolveRF64Size returns the real chunk size, applying ds64 data where the
// 32-bit header uses the sentinel.
func (d *Decoder) resolveRF64Size(id [4]byte, size32 uint32) (int64, error) {
	if size32 == rf64SizeSentinel {
		var real uint64
		switch {
		case id == riff.DataFormatID:
			real = d.ds64.dataSize64
		default:
			var ok bool
			real, ok = d.ds64.chunkSizes[id]
			if !ok {
				return 0, fmt.Errorf("%w: chunk %q", ErrRF64MissingSize, id)
			}
		}
		if real > math.MaxInt64 {
			return 0, fmt.Errorf("%w: chunk %q size %d does not fit on this platform", ErrDS64Contradiction, id, real)
		}
		return int64(real), nil
	}

	// No sentinel: the 32-bit value is authoritative but ds64 must agree
	// wherever it also states a size for this chunk.
	if id == riff.DataFormatID {
		if d.ds64.dataSize64 != uint64(size32) {
			return 0, fmt.Errorf("%w: data chunk size %d disagrees with ds64 data size %d", ErrDS64Contradiction, size32, d.ds64.dataSize64)
		}
	} else if v, ok := d.ds64.chunkSizes[id]; ok && v != uint64(size32) {
		return 0, fmt.Errorf("%w: chunk %q size %d disagrees with ds64 entry %d", ErrDS64Contradiction, id, size32, v)
	}
	return int64(size32), nil
}
