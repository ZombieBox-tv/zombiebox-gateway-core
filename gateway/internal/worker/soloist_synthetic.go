package worker

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os/exec"
	"syscall"
	"time"
)

// CheckSoloistSyntheticOutput exercises the exact production sink and monitor
// inside the isolated runtime image. It uses neither Soloist nor a key/account,
// and writes generated samples only to the private in-memory playback stream.
func CheckSoloistSyntheticOutput(ctx context.Context) error {
	if err := verifySoloistContainer(); err != nil {
		return err
	}
	runtime, err := NewSoloistRuntime(ctx, "synthetic-runtime-token-01234567890123456789")
	if err != nil {
		return err
	}
	defer runtime.Close()
	if err = runtime.startPrivateAudio(ctx); err != nil {
		return err
	}
	reader, err := newSoloistPCMReader(runtime.ctx, "zombiebox_soloist")
	if err != nil {
		return err
	}
	hub, err := NewSoloistPCMHub(runtime.ctx, reader)
	if err != nil {
		_ = reader.Close()
		return err
	}
	defer hub.Close()
	probe, cancel := context.WithTimeout(runtime.ctx, 8*time.Second)
	defer cancel()
	sender := exec.CommandContext(probe, "pw-cat", "--playback", "--raw", "--remote", "zombie-soloist", "--target", "zombiebox_soloist", "--rate", "44100", "--channels", "2", "--format", "s16", "-")
	sender.Stdin = &soloistSyntheticTone{}
	sender.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = sender.Start(); err != nil {
		return errors.New("synthetic sender unavailable")
	}
	defer func() { _ = syscall.Kill(-sender.Process.Pid, syscall.SIGKILL); _ = sender.Wait() }()
	if err = hub.WaitActive(probe); err != nil {
		return errors.New("private PCM output probe failed")
	}
	stream, err := hub.Stream(probe, "synthetic-check")
	if err != nil {
		return err
	}
	defer stream.Close()
	data := make([]byte, 4096)
	if _, err = io.ReadFull(stream, data); err != nil {
		return errors.New("synthetic PCM relay failed")
	}
	signal := false
	for offset := 0; offset < len(data); offset += 2 {
		if binary.LittleEndian.Uint16(data[offset:]) != 0 {
			signal = true
			break
		}
	}
	if !signal {
		return errors.New("synthetic PCM relay was silent")
	}
	return nil
}

type soloistSyntheticTone struct{ frame int64 }

func (t *soloistSyntheticTone) Read(data []byte) (int, error) {
	n := len(data) - len(data)%4
	if n == 0 {
		return 0, io.ErrShortBuffer
	}
	for offset := 0; offset < n; offset += 4 {
		sample := int16(4096 * math.Sin(2*math.Pi*440*float64(t.frame)/44100))
		binary.LittleEndian.PutUint16(data[offset:], uint16(sample))
		binary.LittleEndian.PutUint16(data[offset+2:], uint16(sample))
		t.frame++
	}
	return n, nil
}
