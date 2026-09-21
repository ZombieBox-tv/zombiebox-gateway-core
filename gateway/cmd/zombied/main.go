package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"zombiebox.local/gateway/internal/artwork"
	providerconfig "zombiebox.local/gateway/internal/config"
	"zombiebox.local/gateway/internal/httpclient"
	mediatools "zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/providers"
	"zombiebox.local/gateway/internal/store"

	"zombiebox.local/gateway/internal/server"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "HTTP listen address (trusted LAN only when exposed)")
	check := flag.String("healthcheck", "", "check gateway URL and exit")
	state := flag.String("state", ".local/gateway.db", "private SQLite database path")
	probes := flag.String("probe-dir", "", "directory of synthetic capability fixtures")
	media := flag.String("media-dir", ".local/media", "directory containing local media")
	config := flag.String("config", "", "optional private provider configuration JSON")
	threadfin := flag.String("threadfin-url", "", "optional private Threadfin control base URL")
	relay := flag.String("relay-url", "", "private MediaMTX HLS base URL")
	relayControl := flag.String("relay-control-url", "", "private MediaMTX control API base URL")
	rtspPort := flag.Int("rtsp-port", 8554, "sender-visible RTSP port")
	enableMedia := flag.Bool("media-tools", false, "enable local FFmpeg probing and conversion")
	flag.Parse()
	if *check != "" {
		client := &http.Client{Timeout: 3 * time.Second}
		res, err := client.Get(*check + "/health")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	if *relay != "" && len(os.Getenv("ZOMBIE_RELAY_ADMIN_TOKEN")) < 32 {
		slog.Error("relay requires a private ZOMBIE_RELAY_ADMIN_TOKEN of at least 32 characters")
		os.Exit(1)
	}
	db, err := store.Open(*state)
	if err != nil {
		slog.Error("cannot open state database")
		os.Exit(1)
	}
	defer db.Close()
	pairing := os.Getenv("ZOMBIE_PAIRING_CODE")
	if pairing == "" {
		n, e := rand.Int(rand.Reader, big.NewInt(900000))
		if e != nil {
			panic(e)
		}
		pairing = fmt.Sprintf("%06d", n.Int64()+100000)
	}
	var tools server.Media
	if *enableMedia {
		tools = mediatools.New("ffmpeg", "ffprobe")
	}
	adapters := providers.New(httpclient.Metadata(), httpclient.Private())
	deps := server.Dependencies{YouTubeReceiver: adapters, Artwork: artwork.New(httpclient.Metadata()), Catalog: adapters, Search: adapters, Resolver: adapters, Player: adapters, Browser: adapters, Media: tools, ControlHTTP: httpclient.Private(), StreamHTTP: httpclient.Streaming()}
	app := server.New(db, server.Options{ProbeDir: *probes, ThreadfinURL: *threadfin, PairingCode: pairing, MediaDir: *media, RelayURL: *relay, RelayControlURL: *relayControl, RelayAdminToken: os.Getenv("ZOMBIE_RELAY_ADMIN_TOKEN"), RTSPPort: *rtspPort}, deps)
	if *config != "" {
		configs, e := providerconfig.Load(*config, os.LookupEnv)
		if e != nil {
			slog.Error(e.Error())
			os.Exit(1)
		}
		if e = app.SeedProviders(context.Background(), configs); e != nil {
			slog.Error("invalid provider configuration")
			os.Exit(1)
		}
	}
	fmt.Fprintln(os.Stderr, "Zombie pairing / administration code:", pairing)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *listen, Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		app.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			_ = srv.Close()
		}
	}()
	slog.Info("gateway ready", "listen", *listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server failed", "error", err)
		os.Exit(1)
	}
	// ListenAndServe returns before Shutdown finishes draining active requests.
	<-shutdownDone
}
