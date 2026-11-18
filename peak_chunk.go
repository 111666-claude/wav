package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/go-audio/riff"
)

// CIDPeak is the chunk ID for the PEAK chunk which stores the per-channel
// peak sample values and their positions. It lets applications display
// waveform overviews without scanning the entire audio data.
var CIDPeak = [4]byte{'P', 'E', 'A', 'K'}

// PeakChunkVersion is the version of the PEAK chunk written by this package.
const PeakChunkVersion = 1

var (
	// ErrPeakChannelMismatch is returned when the number of channel peaks
	// stored in a PEAK chunk doesn't match the channel count of the file.
	ErrPeakChannelMismatch = errors.New("PEAK chunk channel count doesn't match the fmt chunk")
	// ErrPeakNonFinite is returned when a peak value (or a sample used to
	// compute one) is NaN or infinite.
	ErrPeakNonFinite = errors.New("non-finite peak sample value")
	// ErrPeakTruncated is returned when a PEAK chunk is truncated or has a
	// malformed size.
	ErrPeakTruncated = errors.New("PEAK chunk is truncated or malformed")
)

// ChannelPeak holds the peak amplitude of a single channel and the position
// (in frames, i.e. per-channel sample index) at which it occurs.
type ChannelPeak struct {
	// Value is the peak amplitude, normalized so that full scale is 1.0.
	Value float32
	// Position is the frame index of the peak within the channel.
	Position uint32
}

// PeakChunk is the structured representation of a RIFF PEAK chunk.
type PeakChunk struct {
	// Version of the PEAK chunk (currently 1).
	Version uint32
	// Timestamp is the Unix time at which the peaks were computed.
	Timestamp uint32
	// Peaks holds one entry per channel.
	Peaks []ChannelPeak
}

// DecodePeakChunk decodes the optional PEAK chunk and stores the result in
// the decoder's Metadata. The number of peak entries is validated against
// the channel count when the fmt chunk has already been parsed.
func DecodePeakChunk(d *Decoder, ch *riff.Chunk) error {
	if ch == nil {
		return fmt.Errorf("can't decode a nil chunk")
	}
	if d == nil {
		return fmt.Errorf("nil decoder")
	}
	if ch.ID != CIDPeak {
		return nil
	}
	if ch.Size < 8 || (ch.Size-8)%8 != 0 {
		return fmt.Errorf("%w: unexpected chunk size %d", ErrPeakTruncated, ch.Size)
	}

	pc := &PeakChunk{}
	if err := binary.Read(ch, binary.LittleEndian, &pc.Version); err != nil {
		return fmt.Errorf("%w: failed to read the version - %v", ErrPeakTruncated, err)
	}
	if err := binary.Read(ch, binary.LittleEndian, &pc.Timestamp); err != nil {
		return fmt.Errorf("%w: failed to read the timestamp - %v", ErrPeakTruncated, err)
	}

	count := (ch.Size - 8) / 8
	if d.NumChans > 0 && count != int(d.NumChans) {
		return fmt.Errorf("%w: %d peaks for %d channel(s)", ErrPeakChannelMismatch, count, d.NumChans)
	}

	pc.Peaks = make([]ChannelPeak, count)
	for i := 0; i < count; i++ {
		if err := binary.Read(ch, binary.LittleEndian, &pc.Peaks[i].Value); err != nil {
			return fmt.Errorf("%w: failed to read the peak value of channel %d - %v", ErrPeakTruncated, i, err)
		}
		if err := binary.Read(ch, binary.LittleEndian, &pc.Peaks[i].Position); err != nil {
			return fmt.Errorf("%w: failed to read the peak position of channel %d - %v", ErrPeakTruncated, i, err)
		}
		if math.IsNaN(float64(pc.Peaks[i].Value)) || math.IsInf(float64(pc.Peaks[i].Value), 0) {
			return fmt.Errorf("%w: channel %d", ErrPeakNonFinite, i)
		}
	}

	if d.Metadata == nil {
		d.Metadata = &Metadata{}
	}
	d.Metadata.Peak = pc

	return nil
}

// ComputePeaks calculates the per-channel peaks of interleaved normalized
// audio samples (full scale = 1.0). It is useful to preview peaks of decoded
// audio without re-encoding. An empty sample slice returns a PeakChunk with
// zeroed entries. Non-finite samples return ErrPeakNonFinite.
func ComputePeaks(samples []float64, numChans int) (*PeakChunk, error) {
	if numChans <= 0 {
		return nil, fmt.Errorf("can't compute peaks for %d channel(s)", numChans)
	}
	pc := &PeakChunk{Version: PeakChunkVersion, Peaks: make([]ChannelPeak, numChans)}
	for i, s := range samples {
		if math.IsNaN(s) || math.IsInf(s, 0) {
			return nil, fmt.Errorf("%w at sample %d", ErrPeakNonFinite, i)
		}
		ch := i % numChans
		if a := math.Abs(s); a > float64(pc.Peaks[ch].Value) {
			pc.Peaks[ch].Value = float32(a)
			pc.Peaks[ch].Position = uint32(i / numChans)
		}
	}
	return pc, nil
}

// normalizedSample converts a raw PCM sample to its absolute normalized
// amplitude (full scale = 1.0) based on the bit depth. 8-bit samples are
// unsigned and centered on 128, all other depths are signed.
func normalizedSample(v int, bitDepth int) float64 {
	switch bitDepth {
	case 8:
		return math.Abs(float64(uint8(v))-128) / 128
	case 16:
		return math.Abs(float64(int16(v))) / 32768
	case 24:
		return math.Abs(float64(v)) / 8388608
	case 32:
		return math.Abs(float64(int32(v))) / 2147483648
	default:
		return 0
	}
}

// frameValueAsInt converts the sample types accepted by Encoder.WriteFrame
// into an int so peaks can be tracked.
func frameValueAsInt(v interface{}) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int16:
		return int(t), true
	case int32:
		return int(t), true
	case uint8:
		return int(t), true
	case uint16:
		return int(t), true
	case uint32:
		return int(t), true
	default:
		return 0, false
	}
}

// initPeakTracker lazily allocates the per-channel peak tracking state.
func (e *Encoder) initPeakTracker() {
	if e.peaks == nil && e.NumChans > 0 {
		e.peaks = make([]float64, e.NumChans)
		e.peakPos = make([]uint32, e.NumChans)
	}
}

// trackPeak folds one normalized sample value into the peak tracker.
func (e *Encoder) trackPeak(ch int, frame uint32, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%w on channel %d", ErrPeakNonFinite, ch)
	}
	if ch < 0 || ch >= len(e.peaks) {
		return nil
	}
	if v > e.peaks[ch] {
		e.peaks[ch] = v
		e.peakPos[ch] = frame
	}
	return nil
}

// writePeakChunk serializes the tracked peaks as a PEAK chunk at the current
// position of the underlying writer. If no audio frames were written, no
// chunk is emitted since there are no peaks to report.
func (e *Encoder) writePeakChunk() error {
	if e.NumChans <= 0 || e.frames == 0 {
		return nil
	}
	e.initPeakTracker()
	ts := e.PeakTimestamp
	if ts == 0 {
		ts = uint32(time.Now().Unix())
	}
	if err := e.AddBE(CIDPeak); err != nil {
		return fmt.Errorf("failed to write the PEAK chunk ID: %w", err)
	}
	if err := e.AddLE(uint32(8 + 8*e.NumChans)); err != nil {
		return fmt.Errorf("failed to write the PEAK chunk size: %w", err)
	}
	if err := e.AddLE(uint32(PeakChunkVersion)); err != nil {
		return fmt.Errorf("failed to write the PEAK chunk version: %w", err)
	}
	if err := e.AddLE(ts); err != nil {
		return fmt.Errorf("failed to write the PEAK chunk timestamp: %w", err)
	}
	for i := 0; i < e.NumChans; i++ {
		if err := e.AddLE(float32(e.peaks[i])); err != nil {
			return fmt.Errorf("failed to write the PEAK value of channel %d: %w", i, err)
		}
		if err := e.AddLE(e.peakPos[i]); err != nil {
			return fmt.Errorf("failed to write the PEAK position of channel %d: %w", i, err)
		}
	}
	return nil
}
