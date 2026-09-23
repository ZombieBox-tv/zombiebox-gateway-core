package vorbis

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"testing"
)

func TestLicensedDecoderMetadataAndSeek(t *testing.T) {
	tone, err := os.ReadFile(os.Getenv("ZOMBIE_SPOTIFY_TEST_OGG"))
	if err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	payload.WriteByte(0x81)
	seek := make([]byte, 1+4+4+1+100)
	seek[0] = segmentTypeSeekTable
	binary.LittleEndian.PutUint32(seek[1:5], 88200)
	binary.LittleEndian.PutUint32(seek[5:9], uint32(len(tone)))
	payload.Write([]byte{byte(len(seek)), 0})
	payload.Write(seek)
	gain := make([]byte, 1+16)
	gain[0] = segmentTypeReplayGain
	payload.Write([]byte{byte(len(gain)), 0})
	payload.Write(gain)
	head := make([]byte, 28)
	copy(head, "OggS")
	head[26] = 1
	head[27] = byte(payload.Len())
	combined := append(append(head, payload.Bytes()...), tone...)
	binary.LittleEndian.PutUint32(combined[22:26], oggCRC(combined[:28+payload.Len()]))
	corrupt := bytes.Clone(combined)
	corrupt[28] ^= 1
	if _, _, err := ExtractMetadataPage(nil, bytes.NewReader(corrupt), int64(len(corrupt))); err == nil {
		t.Fatal("corrupt metadata checksum accepted")
	}
	stream, meta, err := ExtractMetadataPage(nil, bytes.NewReader(combined), int64(len(combined)))
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil || stream.Size() != int64(len(tone)) {
		t.Fatalf("bad extraction %v %d", meta, stream.Size())
	}
	decoder, err := New(nil, stream, meta, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if decoder.SampleRate != 44100 || decoder.Channels != 2 {
		t.Fatalf("format %d/%d", decoder.SampleRate, decoder.Channels)
	}
	samples := make([]float32, 4096)
	n, err := decoder.Read(samples)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no samples")
	}
	peak := float32(0)
	for _, s := range samples[:n] {
		peak = float32(math.Max(float64(peak), math.Abs(float64(s))))
	}
	if peak <= 0 || peak > 0.5 {
		t.Fatalf("gain peak %f", peak)
	}
	if err := decoder.SetPositionMs(1000); err != nil {
		t.Fatal(err)
	}
	if got := decoder.PositionMs(); got < 900 || got > 1100 {
		t.Fatalf("position %d", got)
	}
	n, err = decoder.Read(samples)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no samples after seek")
	}
	decoder.Close()
	if _, err = decoder.Read(samples); err == nil {
		t.Fatal("closed read succeeded")
	}
}
