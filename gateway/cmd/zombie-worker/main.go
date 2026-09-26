// zombie-worker supervises one optional upstream and its bounded media bridge.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"zombiebox.local/gateway/internal/worker"
)

func main() {
	configFile := flag.String("config", "/config/worker.json", "private worker configuration")
	flag.Parse()
	if err := run(*configFile); err != nil {
		log.Fatal(err)
	}
}
func run(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var c worker.Config
	decoder := json.NewDecoder(io.LimitReader(f, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || c.Validate() != nil {
		return fmt.Errorf("invalid worker configuration")
	}
	if bind := os.Getenv("ZOMBIE_SPOTIFY_API_BIND"); bind != "" {
		// Full's host-network Spotify receiver keeps this bearer-only API on
		// Docker's host-gateway interface, never on the physical LAN.
		if c.Mode != "spotify" || bind != "host.docker.internal:8092" {
			return fmt.Errorf("invalid Spotify API bind override")
		}
		c.Listen = bind
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var commands []*exec.Cmd
	var spotifyDiagnostics *worker.SpotifyDaemonDiagnostics
	if c.Mode == "spotify" {
		spotifyDiagnostics = worker.NewSpotifyDaemonDiagnostics()
		fifo := filepath.Join(c.StateDir, "audio.pcm")
		info, statErr := os.Lstat(fifo)
		if os.IsNotExist(statErr) {
			if err = syscall.Mkfifo(fifo, 0600); err != nil {
				return err
			}
		} else if statErr != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return fmt.Errorf("audio output must be a named pipe")
		}
		commands = append(commands, exec.CommandContext(ctx, "go-librespot", "--config_dir", c.StateDir))
	} else {
		hls := filepath.Join(c.StateDir, "hls")
		if err = os.MkdirAll(hls, 0700); err != nil {
			return err
		}
		// UxPlay removes this file when the last client disconnects. Clear a
		// leftover from an unclean prior exit before treating it as evidence.
		dacpPath := filepath.Join(c.StateDir, "receiver.dacp")
		if err = os.Remove(dacpPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear stale receiver state: %w", err)
		}
		audioSDP := filepath.Join(c.StateDir, "audio.sdp")
		if err = os.WriteFile(audioSDP, []byte("v=0\no=- 0 0 IN IP4 127.0.0.1\ns=Zombie AirPlay Audio\nc=IN IP4 127.0.0.1\nt=0 0\nm=audio 35014 RTP/AVP 97\na=rtpmap:97 L16/44100/2\n"), 0600); err != nil {
			return err
		}
		// RTP timestamps may restart when an AirPlay sender changes tracks. Build
		// output timestamps from decoded samples so HLS segment durations remain
		// monotonic. Packet loss/reconnect recovery still needs physical evidence.
		commands = append(commands, exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,udp,rtp", "-localaddr", "127.0.0.1", "-listen_timeout", "-1", "-threads", "1", "-i", audioSDP, "-af", "asetpts=N/SR/TB", "-c:a", "aac", "-threads", "1", "-b:a", "128k", "-f", "hls", "-hls_time", "1", "-hls_list_size", "4", "-hls_flags", "delete_segments+omit_endlist+temp_file", "-hls_segment_filename", filepath.Join(hls, "audio%d.ts"), filepath.Join(hls, "audio.m3u8")))
		// RTP is loopback-only. H.265 is deliberately not advertised to senders.
		sdp := "v=0\no=- 0 0 IN IP4 127.0.0.1\ns=Zombie AirPlay\nc=IN IP4 127.0.0.1\nt=0 0\nm=video 35010 RTP/AVP 96\na=rtpmap:96 H264/90000\na=fmtp:96 packetization-mode=1\nm=audio 35012 RTP/AVP 97\na=rtpmap:97 L16/44100/2\n"
		sdpPath := filepath.Join(c.StateDir, "receiver.sdp")
		if err = os.WriteFile(sdpPath, []byte(sdp), 0600); err != nil {
			return err
		}
		commands = append(commands, exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,udp,rtp", "-localaddr", "127.0.0.1", "-listen_timeout", "-1", "-threads", "1", "-i", sdpPath, "-map", "0:v:0", "-map", "0:a:0", "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-tune", "zerolatency", "-profile:v", "baseline", "-level:v", "3.0", "-vf", "scale=640:360:force_original_aspect_ratio=decrease,pad=640:360:(ow-iw)/2:(oh-ih)/2,format=yuv420p", "-r", "30", "-g", "30", "-b:v", "1000k", "-c:a", "aac", "-b:a", "128k", "-f", "hls", "-hls_time", "1", "-hls_list_size", "4", "-hls_flags", "delete_segments+omit_endlist+temp_file", "-hls_segment_filename", filepath.Join(hls, "segment%d.ts"), filepath.Join(hls, "index.m3u8")))
		commands = append(commands, exec.CommandContext(ctx, "uxplay", "-md", filepath.Join(c.StateDir, "metadata.txt"), "-ca", filepath.Join(c.StateDir, "coverart"), "-dacp", dacpPath, "-n", "Zombie Box AirPlay", "-p", "35000", "-pin", c.Pin, "-s", "1280x720", "-fps", "30", "-vrtp", "pt=96 config-interval=1 ! udpsink host=127.0.0.1 port=35010", "-artp", "pt=97 ! multiudpsink clients=127.0.0.1:35012,127.0.0.1:35014"))
	}
	var wg sync.WaitGroup
	errors := make(chan error, len(commands)+1)
	defer wg.Wait()
	defer cancel()
	for _, cmd := range commands {
		// Upstream logs may contain pairing credentials; they remain off stdout.
		cmd.WaitDelay = time.Second
		if filepath.Base(cmd.Path) == "ffmpeg" {
			cmd.Stderr = os.Stderr
		} else if filepath.Base(cmd.Path) == "go-librespot" {
			cmd.Stderr = spotifyDiagnostics
		}
		if err = cmd.Start(); err != nil {
			return fmt.Errorf("start %s: %w", filepath.Base(cmd.Path), err)
		}
		wg.Add(1)
		go func(cmd *exec.Cmd) {
			defer wg.Done()
			err := cmd.Wait()
			errors <- fmt.Errorf("%s exited: %v", filepath.Base(cmd.Path), err)
		}(cmd)
	}
	httpServer := &http.Server{Addr: c.Listen, Handler: worker.HandlerWithSpotifyDiagnostics(ctx, c, spotifyDiagnostics), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() { errors <- httpServer.ListenAndServe() }()
	var result error
	select {
	case <-ctx.Done():
	case result = <-errors:
	}
	cancel()
	closeContext, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer closeCancel()
	_ = httpServer.Shutdown(closeContext)
	_ = httpServer.Close()
	return result
}
