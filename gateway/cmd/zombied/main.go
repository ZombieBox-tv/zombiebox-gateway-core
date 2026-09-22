package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"zombiebox.local/gateway/internal/artwork"
	"zombiebox.local/gateway/internal/companionmedia"
	providerconfig "zombiebox.local/gateway/internal/config"
	"zombiebox.local/gateway/internal/diagnostics"
	"zombiebox.local/gateway/internal/discovery"
	"zombiebox.local/gateway/internal/httpclient"
	mediatools "zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/providers"
	"zombiebox.local/gateway/internal/store"

	"zombiebox.local/gateway/internal/server"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "HTTP listen address (trusted LAN only when exposed)")
	check := flag.String("healthcheck", "", "check gateway URL and exit")
	stateCopy := flag.String("state-copy", "", "write a consistent private snapshot to a new file, then exit")
	stateCheck := flag.Bool("state-check", false, "check existing state read-only without migration, then exit")
	state := flag.String("state", ".local/gateway.db", "private SQLite database path")
	probes := flag.String("probe-dir", "", "directory of synthetic capability fixtures")
	media := flag.String("media-dir", ".local/media", "directory containing local media")
	config := flag.String("config", "", "optional private provider configuration JSON")
	threadfin := flag.String("threadfin-url", "", "optional private Threadfin control base URL")
	relay := flag.String("relay-url", "", "private MediaMTX HLS base URL")
	relayControl := flag.String("relay-control-url", "", "private MediaMTX control API base URL")
	rtspPort := flag.Int("rtsp-port", 8554, "sender-visible RTSP port")
	enableMedia := flag.Bool("media-tools", false, "enable local FFmpeg probing and conversion")
	artworkDirectory := flag.String("artwork-cache", "", "processed image cache directory (default: artwork beside state database)")
	artworkMiB := flag.Int("artwork-cache-mb", 64, "processed image disk budget in MiB, 0 disables persistence, maximum 512")
	discoveryListen := flag.String("discovery-listen", "", "optional trusted LAN UDP discovery address, typically :8098")
	discoveryPort := flag.Int("discovery-http-port", 8090, "client-visible HTTP port advertised by discovery")
	discoveryOnly := flag.Bool("discovery-only", false, "run discovery only; no database, credentials or HTTP server")
	diagnose := flag.String("diagnose-address", "", "probe one local IPv4 endpoint without loading state or credentials, then exit")
	diagnoseHTTP := flag.Int("diagnose-http-port", 8090, "diagnostic HTTP health port")
	diagnoseUDP := flag.Int("diagnose-discovery-port", 8098, "diagnostic unicast discovery port")
	diagnoseRTSP := flag.Int("diagnose-rtsp-port", 8554, "diagnostic RTSP OPTIONS port")
	flag.Parse()
	if *diagnose != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		report, err := diagnostics.Probe(ctx, &net.Dialer{}, diagnostics.Target{
			Address: *diagnose, HTTPPort: *diagnoseHTTP, DiscoveryPort: *diagnoseUDP, RTSPPort: *diagnoseRTSP,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if json.NewEncoder(os.Stdout).Encode(report) != nil {
			os.Exit(1)
		}
		return
	}
	if *discoveryOnly {
		if *discoveryListen == "" {
			slog.Error("discovery-only requires discovery-listen")
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		socket, err := discovery.Listen(*discoveryListen)
		if err != nil {
			slog.Error("discovery listen failed", "error", err)
			os.Exit(1)
		}
		if err := discovery.Serve(ctx, socket, *discoveryPort); err != nil {
			slog.Error("discovery failed", "error", err)
			os.Exit(1)
		}
		return
	}
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
	if *stateCheck || *stateCopy != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var err error
		if *stateCopy != "" {
			err = store.Snapshot(ctx, *state, *stateCopy)
		} else {
			err = store.Inspect(ctx, *state)
		}
		if err != nil {
			slog.Error("state maintenance failed; existing files are preserved")
			os.Exit(1)
		}
		fmt.Println("State maintenance completed")
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
	var remoteTools server.RemoteMedia
	var remoteSubtitles server.RemoteSubtitles
	if *enableMedia {
		localTools := mediatools.New("ffmpeg", "ffprobe")
		tools = localTools
		remote := mediatools.NewRemote(localTools, httpclient.Streaming())
		remoteTools, remoteSubtitles = remote, remote
	}
	if *artworkMiB < 0 || *artworkMiB > 512 {
		slog.Error("invalid artwork disk cache budget")
		os.Exit(1)
	}
	var artworkCache artwork.Cache
	if *artworkMiB > 0 {
		directory := *artworkDirectory
		if directory == "" {
			directory = filepath.Join(filepath.Dir(*state), "artwork")
		}
		disk, cacheErr := artwork.NewDiskCache(directory, int64(*artworkMiB)<<20)
		if cacheErr != nil {
			slog.Warn("artwork disk cache unavailable; using bounded memory cache")
		} else {
			artworkCache = disk
		}
	}
	var uploads server.MediaUploads
	if *enableMedia {
		disk, uploadErr := companionmedia.New(filepath.Join(filepath.Dir(*state), "companion-media"))
		if uploadErr != nil {
			slog.Warn("phone media uploads unavailable")
		} else {
			uploads = disk
			defer disk.Close()
		}
	}
	adapters := providers.New(httpclient.Metadata(), httpclient.Private())
	deps := server.Dependencies{
		PublicMediaHTTP: httpclient.PublicDownloads(),
		Uploads:         uploads,
		Reception:       adapters,
		Browse:          adapters,
		YouTubeReceiver: adapters,
		Artwork:         artwork.New(httpclient.Metadata(), artworkCache),
		Catalog:         adapters,
		Search:          adapters,
		Resolver:        adapters,
		Player:          adapters,
		Browser:         adapters,
		Media:           tools,
		RemoteMedia:     remoteTools,
		RemoteSubtitles: remoteSubtitles,
		ControlHTTP:     httpclient.Private(),
		StreamHTTP:      httpclient.Streaming(),
	}
	app := server.New(db, server.Options{
		ProbeDir:        *probes,
		ThreadfinURL:    *threadfin,
		PairingCode:     pairing,
		MediaDir:        *media,
		RelayURL:        *relay,
		RelayControlURL: *relayControl,
		RelayAdminToken: os.Getenv("ZOMBIE_RELAY_ADMIN_TOKEN"),
		RTSPPort:        *rtspPort,
	}, deps)
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
	if *discoveryListen != "" {
		socket, err := discovery.Listen(*discoveryListen)
		if err != nil {
			slog.Warn("discovery unavailable; manual pairing remains available", "error", err)
		} else {
			go func() {
				if err := discovery.Serve(ctx, socket, *discoveryPort); err != nil {
					slog.Warn("discovery stopped", "error", err)
				}
			}()
		}
	}
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
