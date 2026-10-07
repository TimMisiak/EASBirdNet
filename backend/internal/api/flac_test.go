package api

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// flacFile is the start of a FLAC holding total samples of 48 kHz 16-bit
// mono, followed by frame bytes, which the server never reads.
func flacFile(total int64, frames int) []byte {
	b := []byte("fLaC\x80\x00\x00\x22") // the last metadata block: STREAMINFO, 34 bytes
	si := make([]byte, 34)
	binary.BigEndian.PutUint16(si[0:], 4096)
	binary.BigEndian.PutUint16(si[2:], 4096)
	binary.BigEndian.PutUint64(si[10:], 48000<<44|0<<41|15<<36|uint64(total))
	return append(append(b, si...), bytes.Repeat([]byte{0xf8}, frames)...)
}

func TestReadStreamInfo(t *testing.T) {
	// The first 42 bytes libFLAC wrote for ten minutes of 48 kHz 16-bit mono
	// (libflac.js 5.6.0), STREAMINFO patched once the encode ended; and the
	// same before it was patched.
	patched, _ := hex.DecodeString("664c614300000022100010000004360011470bb800f001b7740098b457b6339a78880520d21ab274a6aa")
	unpatched, _ := hex.DecodeString("664c614300000022100010000000000000000bb800f00000000000000000000000000000000000000000")

	cases := []struct {
		what string
		in   []byte
		want streamInfo
		err  error
	}{
		{"libFLAC's", patched, streamInfo{SampleRate: 48000, Channels: 1, BitsPerSample: 16, TotalSamples: 28_800_000}, nil},
		{"libFLAC's, unpatched", unpatched, streamInfo{SampleRate: 48000, Channels: 1, BitsPerSample: 16}, nil},
		{"longer than 32 bits", flacFile(1<<34+5, 0), streamInfo{SampleRate: 48000, Channels: 1, BitsPerSample: 16, TotalSamples: 1<<34 + 5}, nil},
		{"a WAV", append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 40)...), streamInfo{}, errNotFLAC},
		{"too short", patched[:30], streamInfo{}, errNotFLAC},
		{"empty", nil, streamInfo{}, errNotFLAC},
		{"another block first", append([]byte("fLaC\x04\x00\x00\x22"), patched[8:]...), streamInfo{}, errNotFLAC},
	}
	for _, tc := range cases {
		got, err := readStreamInfo(bytes.NewReader(tc.in))
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: readStreamInfo = %+v, %v; want %+v, %v", tc.what, got, err, tc.want, tc.err)
		}
	}
}

// flacMeta is the metadata the browser sends with a WAV it sends as FLAC.
func flacMeta(ref, path string, samples int64) map[string]string {
	return map[string]string{
		metaReference: ref, metaPath: path,
		metaEncoding: db.EncodingFLAC, metaSamples: strconv.FormatInt(samples, 10),
	}
}

// A WAV sent as FLAC is received the same as one sent as it was: it counts
// for its length on the card, and the card moves on when it's the last.
func TestTusTakesAWAVAsFLAC(t *testing.T) {
	s := newTusServer(t)
	ctx := t.Context()
	jane := signedIn(t, s.mux, db.RoleVolunteer)
	reg := s.register(jane, cardBody("SW-03", "2026-09-14", "", "2026-09-12", 1, 2))
	ref, first, second := reg.Upload.Reference, reg.Files[0].Path, reg.Files[1].Path

	flac := flacFile(28, 18)
	s.sendWith(jane, flacMeta(ref, first, 28), flac, 50)
	f, err := s.store.GetAudioFile(ctx, ref, db.AudioFileID(ref, first))
	if err != nil || f.Status != db.AudioUploaded || f.Encoding != db.EncodingFLAC || f.StoredBytes != int64(len(flac)) || f.SizeBytes != 100 {
		t.Fatalf("file sent as FLAC = %+v, %v; want uploaded, flac, %d bytes stored of 100 on the card", f, err, len(flac))
	}
	if u := s.card(jane, ref); u.FilesUploaded != 1 || u.BytesUploaded != 100 {
		t.Errorf("card = %d files, %d bytes; want 1, 100: the card's bytes, not the FLAC's", u.FilesUploaded, u.BytesUploaded)
	}

	s.send(jane, ref, second, audio(2), 100)
	if u := s.card(jane, ref); u.Status != db.StatusProcessing || u.BytesUploaded != 200 {
		t.Errorf("after both = %s, %d bytes; want processing, 200", u.Status, u.BytesUploaded)
	}
	raw, _ := s.store.GetAudioFile(ctx, ref, db.AudioFileID(ref, second))
	if raw.Encoding != "" || raw.StoredBytes != 100 {
		t.Errorf("file sent as it was: encoding %q, %d bytes stored; want none, 100", raw.Encoding, raw.StoredBytes)
	}
}

func TestTusRefusesFLACItCantTake(t *testing.T) {
	s := newTusServer(t)
	jane := signedIn(t, s.mux, db.RoleVolunteer)
	reg := s.register(jane, oneNightBody("SW-03", "2026-09-14", "2026-09-12", []CardFile{
		{Path: "DATA/a.WAV", Bytes: 1000, Night: "2026-09-12"},
		{Path: "DATA/b.mp3", Bytes: 1000, Night: "2026-09-12"},
	}))
	ref := reg.Upload.Reference
	with := func(meta map[string]string, change func(map[string]string)) map[string]string {
		change(meta)
		return meta
	}

	cases := []struct {
		what string
		meta map[string]string
		size int64
	}{
		{"no sample count", with(flacMeta(ref, "DATA/a.WAV", 1), func(m map[string]string) { delete(m, metaSamples) }), 100},
		{"a sample count that isn't one", with(flacMeta(ref, "DATA/a.WAV", 1), func(m map[string]string) { m[metaSamples] = "-3" }), 100},
		{"more samples than the WAV has bytes", flacMeta(ref, "DATA/a.WAV", 1001), 100},
		{"something other than a WAV", flacMeta(ref, "DATA/b.mp3", 100), 100},
		{"an encoding it doesn't take", with(flacMeta(ref, "DATA/a.WAV", 100), func(m map[string]string) { m[metaEncoding] = "opus" }), 100},
		{"shorter than a FLAC header", flacMeta(ref, "DATA/a.WAV", 100), flacHeaderBytes - 1},
		{"longer than a FLAC of the WAV", flacMeta(ref, "DATA/a.WAV", 100), maxFLACBytes(1000) + 1},
	}
	for _, tc := range cases {
		if r := s.createWith(jane, tc.meta, tc.size); r.status != http.StatusBadRequest {
			t.Errorf("%s: create = %d (%s), want 400", tc.what, r.status, strings.TrimSpace(r.body))
		}
	}
	if r := s.createWith(jane, flacMeta(ref, "DATA/a.WAV", 100), maxFLACBytes(1000)); r.status != http.StatusCreated {
		t.Errorf("the longest FLAC the WAV could make: create = %d (%s), want 201", r.status, r.body)
	}
}

// What makes a FLAC whole is checked once its last byte lands: one that
// isn't is refused and its bytes deleted, and the file can be sent again.
func TestTusRefusesAFLACThatIsntWhole(t *testing.T) {
	s := newTusServer(t)
	ctx := t.Context()
	jane := signedIn(t, s.mux, db.RoleVolunteer)
	reg := s.register(jane, cardBody("SW-03", "2026-09-14", "", "2026-09-12", 1, 1))
	ref, path := reg.Upload.Reference, reg.Files[0].Path

	cases := []struct {
		what string
		data []byte
		want string
	}{
		{"an encode cut short", flacFile(27, 18), "holds 27 samples, not the 28"},
		{"a header never patched", flacFile(0, 18), "doesn't say how long"},
		{"not a FLAC at all", audio(3)[:60], "isn't a FLAC"},
	}
	for _, tc := range cases {
		created := s.createWith(jane, flacMeta(ref, path, 28), int64(len(tc.data)))
		if created.status != http.StatusCreated {
			t.Fatalf("%s: create = %d (%s)", tc.what, created.status, created.body)
		}
		location := created.header.Get("Location")
		r := s.sendTo(jane, location, tc.data, 50)
		if r.status != http.StatusBadRequest || !strings.Contains(r.body, "ERR_FILE_FLAC") || !strings.Contains(r.body, tc.want) {
			t.Errorf("%s: last patch = %d (%s), want 400 ERR_FILE_FLAC saying %q", tc.what, r.status, strings.TrimSpace(r.body), tc.want)
		}
		id := location[strings.Index(location, tusPath)+len(tusPath):]
		if _, err := s.files.Open(ctx, storage.Name(id)); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("%s: the refused upload's bytes: err = %v, want ErrNotFound", tc.what, err)
		}
		if f, _ := s.store.GetAudioFile(ctx, ref, db.AudioFileID(ref, path)); f.Status != db.AudioPending || f.BlobName != "" {
			t.Errorf("%s: file = %s at %q, want pending with nothing stored", tc.what, f.Status, f.BlobName)
		}
	}
	if u := s.card(jane, ref); u.FilesUploaded != 0 {
		t.Errorf("refused FLACs counted %d files", u.FilesUploaded)
	}

	// The same file, whole, goes in.
	s.sendWith(jane, flacMeta(ref, path, 28), flacFile(28, 18), 50)
	if u := s.card(jane, ref); u.FilesUploaded != 1 || u.Status != db.StatusProcessing {
		t.Errorf("after a whole FLAC: %d files, %s; want 1, processing", u.FilesUploaded, u.Status)
	}
}
