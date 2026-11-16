# wav codec

[![GoDoc](https://godoc.org/github.com/go-audio/wav?status.svg)](https://godoc.org/github.com/go-audio/wav)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-audio/wav)](https://goreportcard.com/report/github.com/go-audio/wav)
[![Coverage Status](https://codecov.io/gh/go-audio/wav/graph/badge.svg)](https://codecov.io/gh/go-audio/wav)
[![Build Status](https://travis-ci.org/go-audio/wav.svg)](https://travis-ci.org/go-audio/wav)

Wav audio file encoder and decoder. See [GoDoc](https://godoc.org/github.com/go-audio/wav) for more details.

## Broadcast Wave Format (bext)

The package reads and writes the version 0 `bext` chunk defined by
[EBU Tech 3285](https://tech.ebu.ch/publications/tech3285). BWF versions 1 and
2 are accepted when reading; their UMID and loudness extension fields are
skipped so that `CodingHistory` is always exposed.

Set `Encoder.Bext` to write broadcast metadata; oversized fixed fields,
malformed dates/times and unrepresentable time references fail with an error
instead of being truncated:

```go
enc := wav.NewEncoder(out, 48000, 16, 1, 1)
enc.Bext = &wav.Bext{
    Description:         "field recording",
    Originator:          "Radio Nature Unit",
    OriginatorReference: "RNU-2026-0001",
    OriginationDate:     "2026-11-16", // YYYY-MM-DD
    OriginationTime:     "09:30:00",   // HH:MM:SS
    CodingHistory:       "A=PCM,F=48000,W=16,M=mono\r\n",
}
if err := enc.Bext.SetTimeReference(90*time.Second, 48000); err != nil {
    log.Fatal(err) // sets TimeReference to 4 320 000 samples
}
```

Read it back through `Decoder.Bext` after `ReadMetadata()` (or after
`FwdToPCM()` when the chunk sits before the audio data).

