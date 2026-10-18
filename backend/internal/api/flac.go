package api

import (
	"encoding/binary"
	"errors"
	"io"
)

// A WAV may be sent as FLAC: the browser encodes it before the upload starts
// and says so in the upload's metadata (tus.go). What the server reads of it
// is the header, which is all it needs to know the encode was finished.

// streamInfo is what a FLAC's STREAMINFO block says about the audio in it.
type streamInfo struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	// TotalSamples is per channel. 0 means the encoder didn't say, which a
	// stream encoder leaves there unless it is patched once the encode ends.
	TotalSamples int64
}

// flacHeaderBytes is the start of every FLAC file: "fLaC", then the header
// of the first metadata block, which the format requires to be STREAMINFO,
// then its 34 bytes.
const flacHeaderBytes = 4 + 4 + 34

// maxFLACBytes is the most a FLAC of a WAV that long can take. FLAC stores a
// block it can't predict verbatim, so noise comes out a little longer than it
// went in: a frame header and a CRC per block, under 1% at the smallest block
// libFLAC's presets use, plus the metadata blocks in front.
func maxFLACBytes(wavBytes int64) int64 {
	return wavBytes + wavBytes/64 + 64<<10
}

var errNotFLAC = errors.New("not a FLAC file")

// readStreamInfo reads a FLAC's STREAMINFO from the start of the file. A file
// too short to hold one, or that doesn't start with one, is errNotFLAC; any
// other error is the reader's.
func readStreamInfo(r io.Reader) (streamInfo, error) {
	var b [flacHeaderBytes]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return streamInfo{}, errNotFLAC
		}
		return streamInfo{}, err
	}
	// The block header: last-block flag and type 0 (STREAMINFO), then a
	// 24-bit length.
	if string(b[:4]) != "fLaC" || b[4]&0x7f != 0 || int(b[5])<<16|int(b[6])<<8|int(b[7]) != 34 {
		return streamInfo{}, errNotFLAC
	}
	// STREAMINFO's bytes 10-17 pack, big-endian: the sample rate (20 bits),
	// channels - 1 (3), bits per sample - 1 (5) and total samples (36).
	x := binary.BigEndian.Uint64(b[8+10 : 8+18])
	return streamInfo{
		SampleRate:    int(x >> 44),
		Channels:      int(x>>41&0x7) + 1,
		BitsPerSample: int(x>>36&0x1f) + 1,
		TotalSamples:  int64(x & (1<<36 - 1)),
	}, nil
}
