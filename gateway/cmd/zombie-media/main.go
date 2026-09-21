// zombie-media is the native media-wrapper diagnostic entrypoint, shared by Full hosts and Edge.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"zombiebox.local/gateway/internal/media"
)

func main() {
	input := flag.String("input", "", "local media file")
	mode := flag.String("mode", "PROBE", "PROBE, REMUX or TRANSCODE")
	output := flag.String("output", "", "new output MP4 file; never overwrite")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tools := media.New("ffmpeg", "ffprobe")
	var err error
	if *mode == "PROBE" {
		var metadata media.Metadata
		metadata, err = tools.Probe(ctx, *input)
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(metadata)
		}
	} else {
		var file *os.File
		file, err = os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			err = tools.Convert(ctx, *input, *mode, file)
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				_ = os.Remove(*output)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Media operation failed:", err)
		os.Exit(1)
	}
}
