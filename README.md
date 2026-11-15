# wav codec

[![GoDoc](https://godoc.org/github.com/go-audio/wav?status.svg)](https://godoc.org/github.com/go-audio/wav)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-audio/wav)](https://goreportcard.com/report/github.com/go-audio/wav)
[![Coverage Status](https://codecov.io/gh/go-audio/wav/graph/badge.svg)](https://codecov.io/gh/go-audio/wav)
[![Build Status](https://travis-ci.org/go-audio/wav.svg)](https://travis-ci.org/go-audio/wav)

Wav audio file encoder and decoder. See [GoDoc](https://godoc.org/github.com/go-audio/wav) for more details.

## WAVE_FORMAT_EXTENSIBLE

Multi-channel files need explicit speaker positions so importers do not guess
the channel order. Set `Encoder.ChannelMask` to write a
WAVE_FORMAT_EXTENSIBLE file with integer PCM data. The encoder then writes a
40 byte `fmt ` chunk containing the PCM sub-format GUID
(`KSDATAFORMAT_SUBTYPE_PCM`), the channel mask and the valid bit depth.
When `ChannelMask` is left at zero, the standard 16 byte `fmt ` chunk is
written and the legacy behavior is unchanged.

```go
e := wav.NewEncoder(out, 48000, 24, 6, wav.WAVFormatPCM)
e.ChannelMask = wav.ChannelMask5Point1 // FL, FR, FC, LFE, BL, BR
// optional: e.ValidBitsPerSample = 20 // defaults to BitDepth
```

On decode, `Decoder.IsExtensible` reports the extended format and
`ChannelMask`, `ValidBitsPerSample` and `SubFormat` expose the extension
fields. The interleaved PCM sample order is unchanged.
