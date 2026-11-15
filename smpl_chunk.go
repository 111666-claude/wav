package wav

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/go-audio/riff"
)

// smpl chunk is documented here:
// https://sites.google.com/site/musicgapi/technical-documents/wav-file-format#smpl

// AddSampleLoop adds a sample loop to the sampler info of the file being
// encoded, allocating the sampler info if needed. The loops are serialized
// when Close is called. The loop must reference the ID of an existing cue
// point and its start must not be greater than its end, Close reports an
// error otherwise.
func (e *Encoder) AddSampleLoop(loop *SampleLoop) error {
	if e == nil {
		return fmt.Errorf("can't add a sample loop to a nil encoder")
	}
	if e.closed {
		return ErrEncoderClosed
	}
	if loop == nil {
		return fmt.Errorf("can't add a nil sample loop")
	}
	if loop.Start > loop.End {
		return fmt.Errorf("%w: start %d > end %d", ErrInvalidLoopRange, loop.Start, loop.End)
	}
	if e.Metadata == nil {
		e.Metadata = &Metadata{}
	}
	if e.Metadata.SamplerInfo == nil {
		e.Metadata.SamplerInfo = &SamplerInfo{}
	}
	e.Metadata.SamplerInfo.Loops = append(e.Metadata.SamplerInfo.Loops, loop)
	e.Metadata.SamplerInfo.NumSampleLoops = uint32(len(e.Metadata.SamplerInfo.Loops))
	return nil
}

// writeSamplerInfo serializes the sampler info of the encoder's metadata as
// a smpl chunk. The loops are validated in Close before this is called.
func (e *Encoder) writeSamplerInfo() error {
	si := e.Metadata.SamplerInfo
	si.NumSampleLoops = uint32(len(si.Loops))
	if err := e.AddBE(CIDSmpl); err != nil {
		return fmt.Errorf("failed to write the smpl chunk ID: %w", err)
	}
	size := uint32(36 + 24*len(si.Loops))
	if err := e.AddLE(size); err != nil {
		return fmt.Errorf("failed to write the smpl chunk size: %w", err)
	}
	if err := e.AddLE(si.Manufacturer); err != nil {
		return fmt.Errorf("failed to write the smpl manufacturer: %w", err)
	}
	if err := e.AddLE(si.Product); err != nil {
		return fmt.Errorf("failed to write the smpl product: %w", err)
	}
	if err := e.AddLE(si.SamplePeriod); err != nil {
		return fmt.Errorf("failed to write the smpl sample period: %w", err)
	}
	if err := e.AddLE(si.MIDIUnityNote); err != nil {
		return fmt.Errorf("failed to write the smpl MIDI unity note: %w", err)
	}
	if err := e.AddLE(si.MIDIPitchFraction); err != nil {
		return fmt.Errorf("failed to write the smpl MIDI pitch fraction: %w", err)
	}
	if err := e.AddLE(si.SMPTEFormat); err != nil {
		return fmt.Errorf("failed to write the smpl SMPTE format: %w", err)
	}
	if err := e.AddLE(si.SMPTEOffset); err != nil {
		return fmt.Errorf("failed to write the smpl SMPTE offset: %w", err)
	}
	if err := e.AddLE(si.NumSampleLoops); err != nil {
		return fmt.Errorf("failed to write the smpl number of loops: %w", err)
	}
	// no sampler specific data follows the loops
	if err := e.AddLE(uint32(0)); err != nil {
		return fmt.Errorf("failed to write the smpl sampler data size: %w", err)
	}
	for _, l := range si.Loops {
		if err := e.AddLE(l.CuePointID); err != nil {
			return fmt.Errorf("failed to write a sample loop cue point ID: %w", err)
		}
		if err := e.AddLE(l.Type); err != nil {
			return fmt.Errorf("failed to write a sample loop type: %w", err)
		}
		if err := e.AddLE(l.Start); err != nil {
			return fmt.Errorf("failed to write a sample loop start: %w", err)
		}
		if err := e.AddLE(l.End); err != nil {
			return fmt.Errorf("failed to write a sample loop end: %w", err)
		}
		if err := e.AddLE(l.Fraction); err != nil {
			return fmt.Errorf("failed to write a sample loop fraction: %w", err)
		}
		if err := e.AddLE(l.PlayCount); err != nil {
			return fmt.Errorf("failed to write a sample loop play count: %w", err)
		}
	}
	return e.writeAlignmentPad(size)
}

// DecodeSamplerChunk decodes a smpl chunk and put the data in Decoder.Metadata.SamplerInfo
func DecodeSamplerChunk(d *Decoder, ch *riff.Chunk) error {
	if ch == nil {
		return fmt.Errorf("can't decode a nil chunk")
	}
	if d == nil {
		return fmt.Errorf("nil decoder")
	}
	if ch.ID == CIDSmpl {
		// read the entire chunk in memory
		buf := make([]byte, ch.Size)
		var err error
		if _, err = ch.Read(buf); err != nil {
			return fmt.Errorf("failed to read the smpl chunk - %w", err)
		}
		if d.Metadata == nil {
			d.Metadata = &Metadata{}
		}

		d.Metadata.SamplerInfo = &SamplerInfo{}

		r := bytes.NewReader(buf)

		scratch := make([]byte, 4)
		if _, err = r.Read(scratch); err != nil {
			return fmt.Errorf("failed to read the smpl Manufacturer")
		}
		copy(d.Metadata.SamplerInfo.Manufacturer[:], scratch[:4])
		if _, err = r.Read(scratch); err != nil {
			return fmt.Errorf("failed to read the smpl Product")
		}
		copy(d.Metadata.SamplerInfo.Product[:], scratch[:4])

		if err := binary.Read(r, binary.LittleEndian, &d.Metadata.SamplerInfo.SamplePeriod); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &d.Metadata.SamplerInfo.MIDIUnityNote); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &d.Metadata.SamplerInfo.MIDIPitchFraction); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &d.Metadata.SamplerInfo.SMPTEFormat); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &d.Metadata.SamplerInfo.SMPTEOffset); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &d.Metadata.SamplerInfo.NumSampleLoops); err != nil {
			return err
		}
		var remaining uint32
		// sampler data
		if err := binary.Read(r, binary.BigEndian, &remaining); err != nil {
			return err
		}
		if d.Metadata.SamplerInfo.NumSampleLoops > 0 {
			d.Metadata.SamplerInfo.Loops = []*SampleLoop{}
			for i := uint32(0); i < d.Metadata.SamplerInfo.NumSampleLoops; i++ {
				sl := &SampleLoop{}
				if _, err = r.Read(scratch); err != nil {
					return fmt.Errorf("failed to read the sample loop cue point id")
				}
				copy(sl.CuePointID[:], scratch[:4])
				if err := binary.Read(r, binary.LittleEndian, &sl.Type); err != nil {
					return err
				}
				if err := binary.Read(r, binary.LittleEndian, &sl.Start); err != nil {
					return err
				}
				if err := binary.Read(r, binary.LittleEndian, &sl.End); err != nil {
					return err
				}
				if err := binary.Read(r, binary.LittleEndian, &sl.Fraction); err != nil {
					return err
				}
				if err := binary.Read(r, binary.LittleEndian, &sl.PlayCount); err != nil {
					return err
				}

				d.Metadata.SamplerInfo.Loops = append(d.Metadata.SamplerInfo.Loops, sl)
			}
		}
	}
	ch.Drain()
	return nil
}
