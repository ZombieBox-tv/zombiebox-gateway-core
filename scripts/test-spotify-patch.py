#!/usr/bin/env python3
"""Check the pinned Spotify patch with synthetic metadata, decode, gain and seek."""

import os
import pathlib
import shutil
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
PREPARE = ROOT / "scripts/prepare-spotify-source.py"
TEST = ROOT / "wrappers/spotify/patches/decoder_integration_test.go"
KEY_REFUSAL_TEST = ROOT / "wrappers/spotify/patches/key_refusal_test.go"
SOURCE = ROOT / "third_party/sources/go-librespot"

ALSA_STUB = """//go:build android || darwin || js || windows || nintendosdk || !cgo

package output

import "fmt"

func newAlsaOutput(opts *NewOutputOptions) (Output, error) {
\treturn nil, fmt.Errorf("alsa output is not supported on this platform")
}
"""

FLAC_STUB = """//go:build !cgo

package flac

import (
\t"errors"
\t"io"

\tlibrespot "github.com/devgianlu/go-librespot"
)

type Decoder struct {
\tSampleRate int32
\tChannels   int32
\tBitDepth   int32
}

func (d *Decoder) Read(_ []float32) (int, error) { return 0, io.EOF }
func (d *Decoder) Close() error { return nil }
func (d *Decoder) PositionMs() int64 { return 0 }
func (d *Decoder) SetPositionMs(_ int64) error { return nil }

func New(_ librespot.Logger, _ io.Reader, _ float32) (*Decoder, error) {
\treturn nil, errors.New("flac not supported without cgo")
}
"""

MP3_STUB = """//go:build !cgo

package mp3

import (
\t"errors"
\t"io"

\tlibrespot "github.com/devgianlu/go-librespot"
)

type Decoder struct {
\tSampleRate int32
\tChannels   int32
}

func (d *Decoder) Read(_ []float32) (int, error) { return 0, io.EOF }
func (d *Decoder) Close() error { return nil }
func (d *Decoder) PositionMs() int64 { return 0 }
func (d *Decoder) SetPositionMs(_ int64) error { return nil }

func New(_ librespot.Logger, _ io.Reader, _ float32) (*Decoder, error) {
\treturn nil, errors.New("mp3 not supported without cgo")
}
"""


def main() -> None:
    if not SOURCE.is_dir():
        raise SystemExit("Run make references before testing the Spotify patch")
    with tempfile.TemporaryDirectory(prefix="zombie-spotify-patch-") as temporary:
        root = pathlib.Path(temporary)
        staged = root / "go-librespot"
        tone = root / "tone.ogg"
        subprocess.run(
            ["python3", str(PREPARE), "--source", str(SOURCE), "--output", str(staged)],
            check=True,
        )
        subprocess.run(
            [
                "ffmpeg",
                "-hide_banner",
                "-loglevel",
                "error",
                "-f",
                "lavfi",
                "-i",
                "sine=frequency=440:duration=2",
                "-c:a",
                "libvorbis",
                "-ac",
                "2",
                "-y",
                str(tone),
            ],
            check=True,
        )
        shutil.copy2(TEST, staged / "vorbis/decoder_integration_test.go")
        environment = dict(
            os.environ, ZOMBIE_SPOTIFY_TEST_OGG=str(tone), GOMAXPROCS="2"
        )
        subprocess.run(
            ["go", "test", "-mod=readonly", "-p", "2", "./vorbis"],
            cwd=staged,
            env=environment,
            check=True,
        )
        modules = subprocess.check_output(
            ["go", "list", "-m", "all"], cwd=staged, env=environment, text=True
        )
        if "github.com/xlab/vorbis-go" in modules:
            raise RuntimeError("Unlicensed Vorbis binding remains in module graph")

        (staged / "output/driver-alsa-stub.go").write_text(ALSA_STUB)
        (staged / "flac/decoder_stub.go").write_text(FLAC_STUB)
        (staged / "mp3/decoder_stub.go").write_text(MP3_STUB)
        shutil.copy2(KEY_REFUSAL_TEST, staged / "daemon/key_refusal_test.go")
        daemon_env = dict(environment, CGO_ENABLED="0")
        subprocess.run(
            [
                "go",
                "test",
                "-mod=readonly",
                "-tags",
                "test_unit",
                "-p",
                "2",
                "./daemon",
                "-run",
                "TestIsUnplayableMedia|TestKeyRefusal|TestRestrictedAndUnsupportedMedia",
            ],
            cwd=staged,
            env=daemon_env,
            check=True,
        )


if __name__ == "__main__":
    main()
