# wav codec

[![GoDoc](https://godoc.org/github.com/go-audio/wav?status.svg)](https://godoc.org/github.com/go-audio/wav)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-audio/wav)](https://goreportcard.com/report/github.com/go-audio/wav)
[![Coverage Status](https://codecov.io/gh/go-audio/wav/graph/badge.svg)](https://codecov.io/gh/go-audio/wav)
[![Build Status](https://travis-ci.org/go-audio/wav.svg)](https://travis-ci.org/go-audio/wav)

Wav audio file encoder and decoder. See [GoDoc](https://godoc.org/github.com/go-audio/wav) for more details.

## RF64 (files larger than 4 GiB)

Classic RIFF WAV files store their sizes in 32-bit fields and are therefore
limited to 4 GiB. RF64 (EBU Tech 3306 / BW64) keeps the same `fmt ` and `data`
chunks but adds a mandatory `ds64` chunk holding the 64-bit file size, data
size and sample-frame count, while the 32-bit fields become `0xFFFFFFFF`
placeholders.

RF64 output is an explicit opt-in; the default remains classic RIFF and is
byte-compatible with previous releases:

```go
e := wav.NewRF64Encoder(f, sampleRate, bitDepth, numChans, audioFormat)
// or: wav.NewEncoder(f, ..., wav.WithRF64())
// or: e := wav.NewEncoder(f, ...); e.SetRF64(true) // before the first write
```

Small files may use RF64 too. The mode is fixed once the header is written
(`ErrModeLocked` otherwise). `Close` reports `ErrNoData` when nothing was
written, `ErrSizeOverflow` when a classic RIFF file would exceed 4 GiB, and
`ErrUnseekable` when the writer can't backfill the sizes; repeated `Close`
calls return the same result.

