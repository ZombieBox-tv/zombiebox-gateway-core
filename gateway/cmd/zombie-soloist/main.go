// zombie-soloist owns the optional isolated Soloist/PipeWire runtime.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"zombiebox.local/gateway/internal/worker"
)

func main() {
	config := flag.String("config", "/config/worker.json", "private Spotify worker configuration")
	synthetic := flag.Bool("synthetic-check", false, "check isolated sink/capture using a generated tone, without Soloist or a key")
	flag.Parse()
	if *synthetic {
		if err := worker.CheckSoloistSyntheticOutput(context.Background()); err != nil {
			log.Fatal(err)
		}
		log.Print("PASS: private PipeWire sink produced advancing non-silent s16le stereo 44100 Hz PCM; no Spotify/device acceptance")
		return
	}
	if err := run(*config); err != nil {
		log.Fatal(err)
	}
}
func run(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.New("Soloist configuration unavailable")
	}
	defer file.Close()
	var c worker.Config
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || decoder.Decode(new(any)) != io.EOF || c.Validate() != nil || c.Mode != "spotify" {
		return errors.New("invalid Soloist configuration")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	runtime, err := worker.NewSoloistRuntime(ctx, c.Token)
	if err != nil {
		return err
	}
	defer runtime.Close()
	iface, err := net.InterfaceByName("api0")
	if err != nil {
		return errors.New("private Soloist API interface unavailable")
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return errors.New("private Soloist API address unavailable")
	}
	bind := ""
	for _, address := range addresses {
		if network, ok := address.(*net.IPNet); ok && network.IP.To4() != nil {
			bind = net.JoinHostPort(network.IP.String(), "8097")
			break
		}
	}
	if bind == "" {
		return errors.New("private Soloist IPv4 address unavailable")
	}
	server := &http.Server{Addr: bind, Handler: runtime, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-done:
	case <-runtime.Done():
		err = errors.New("Soloist runtime stopped")
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	_ = server.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
