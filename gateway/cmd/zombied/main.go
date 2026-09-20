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
	providerconfig "zombiebox.local/gateway/internal/config"
	"zombiebox.local/gateway/internal/store"

	"zombiebox.local/gateway/internal/server"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "HTTP listen address (trusted LAN only when exposed)")
	check := flag.String("healthcheck", "", "check gateway URL and exit")
	state := flag.String("state", ".local/gateway.db", "private SQLite database path")
	media := flag.String("media-dir", ".local/media", "directory containing local media")
	config := flag.String("config", "", "optional private provider configuration JSON")
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
	app := server.New(db, server.Options{PairingCode: pairing, MediaDir: *media})
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
