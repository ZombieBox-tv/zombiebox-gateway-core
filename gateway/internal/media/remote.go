package media

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media/manifest"
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// RemoteTools keeps provider URLs/headers out of process arguments. FFmpeg sees
// only an ephemeral loopback origin with opaque progressive routes or a bounded manifest graph.
type RemoteTools struct {
	tools *Tools
	http  HTTPClient
}

func NewRemote(tools *Tools, client HTTPClient) *RemoteTools {
	return &RemoteTools{tools: tools, http: client}
}

func (t *RemoteTools) ProbeRemote(ctx context.Context, source domain.Source) (domain.Metadata, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	bridge, err := t.bridge(ctx, source)
	if err != nil {
		return domain.Metadata{}, err
	}
	defer bridge.close()
	metadata, err := t.tools.probe(ctx, bridge.video, true, bridge.kind)
	if err != nil {
		return metadata, bridge.failureOr(err)
	}
	if err := bridge.failureOr(nil); err != nil {
		return metadata, err
	}
	if bridge.audio != "" {
		audio, err := t.tools.probe(ctx, bridge.audio, true)
		if err != nil {
			return metadata, bridge.failureOr(err)
		}
		if err := bridge.failureOr(nil); err != nil {
			return metadata, err
		}
		for _, stream := range audio.Streams {
			if stream.Type == "audio" {
				metadata.Streams = append(metadata.Streams, stream)
			}
		}
	}
	return metadata, nil
}

func (t *RemoteTools) ConvertRemote(ctx context.Context, source domain.Source, mode string, selection domain.MediaSelection, output io.Writer) error {
	if source.Live && selection.PositionMS != 0 {
		return errors.New("live input is not seekable")
	}
	pcmStream := mode == "PCM_STREAM"
	if pcmStream && (!source.Live || (ManifestKind(source) != "hls" && !IsLiveMP3Source(source))) {
		return errors.New("PCM stream requires supported live audio")
	}
	bridge, err := t.bridge(ctx, source)
	if err != nil {
		return err
	}
	defer bridge.close()
	if pcmStream && bridge.kind != "hls" && !IsLiveMP3Source(source) {
		return errors.New("PCM stream requires supported live audio")
	}
	// TS AAC carries ADTS headers; copying to fragmented MP4 needs ASC.
	// Manifests may contain TS. Never apply an AAC filter to MP3/other audio.
	adtsAAC := false
	liveAudio := false
	needsProbe := pcmStream || (mode == "REMUX" &&
		(bridge.kind != "" || strings.EqualFold(strings.TrimSpace(strings.Split(source.MIME, ";")[0]), "video/mp2t")))
	if needsProbe {
		metadata, err := t.tools.probe(ctx, bridge.video, true, bridge.kind)
		if err != nil {
			return bridge.failureOr(err)
		}
		if err := bridge.failureOr(nil); err != nil {
			return err
		}
		hasAudio, hasVideo := false, false
		selectedAudio := -1
		selectedCodec := ""
		for _, stream := range metadata.Streams {
			if stream.Type == "video" {
				hasVideo = true
			}
			if stream.Type == "audio" {
				if selection.AudioID == nil && selectedAudio < 0 {
					selectedAudio = stream.Index
					selectedCodec = stream.Codec
				}
				if selection.AudioID != nil && stream.Index == *selection.AudioID {
					selectedAudio = stream.Index
					selectedCodec = stream.Codec
				}
				if selection.AudioID == nil || stream.Index == *selection.AudioID {
					hasAudio = true
					adtsAAC = stream.Codec == "aac"
				}
			}
		}
		if pcmStream {
			if hasVideo || selectedAudio < 0 {
				return errors.New("PCM stream requires live audio-only input")
			}
			if IsLiveMP3Source(source) && selectedCodec != "mp3" {
				return errors.New("live MP3 source did not contain MP3 audio")
			}
			selection.AudioID = &selectedAudio
		}
		liveAudio = source.Live && hasAudio && adtsAAC && !hasVideo
	}
	err = t.tools.convert(ctx, bridge.video, bridge.audio, true, adtsAAC, liveAudio, mode, selection, output, bridge.kind)
	return bridge.failureOr(err)
}

type inputBridge struct {
	video, audio string
	kind         string
	close        func()
	failures     *upstreamFailureState
}

func (b inputBridge) failureOr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if b.failures != nil {
		if failure := b.failures.failure(); failure != nil {
			return failure
		}
	}
	return err
}

type upstreamFailureState struct {
	mu    sync.Mutex
	first *ConversionFailure
}

func (s *upstreamFailureState) record(class string, status int) {
	if s == nil {
		return
	}
	switch class {
	case ConversionFailureUpstreamHTTP, ConversionFailureUpstreamTimeout,
		ConversionFailureUpstreamTransport, ConversionFailureUpstreamRange:
	default:
		class = ConversionFailureUpstreamTransport
	}
	if class != ConversionFailureUpstreamHTTP || status < 100 || status > 599 {
		status = 0
		if class == ConversionFailureUpstreamHTTP {
			class = ConversionFailureUpstreamTransport
		}
	}
	failure := newConversionFailure(class, 0, status)
	s.mu.Lock()
	if s.first == nil {
		s.first = failure
	}
	s.mu.Unlock()
}

func (s *upstreamFailureState) failure() *ConversionFailure {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first == nil {
		return nil
	}
	copy := *s.first
	return &copy
}

func upstreamErrorClass(err error, fallback string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return ConversionFailureUpstreamTimeout
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return ConversionFailureUpstreamTimeout
	}
	return fallback
}

type upstreamBodyReader struct {
	io.Reader
	readError error
}

func (r *upstreamBodyReader) Read(buffer []byte) (int, error) {
	count, err := r.Reader.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		r.readError = err
	}
	return count, err
}

func copyUpstreamBody(destination io.Writer, source io.Reader) error {
	body := &upstreamBodyReader{Reader: source}
	_, _ = io.Copy(destination, body)
	return body.readError
}

func (t *RemoteTools) bridge(ctx context.Context, source domain.Source) (inputBridge, error) {
	if !RemoteCandidate(source) {
		return inputBridge{}, errors.New("remote input unsupported")
	}
	if kind := ManifestKind(source); kind != "" {
		proxy, err := manifest.Open(ctx, t.http, source.URL, kind, source.Headers)
		if err != nil {
			return inputBridge{}, err
		}
		return inputBridge{video: proxy.URL, kind: kind, close: proxy.Close}, nil
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return inputBridge{}, errors.New("media input unavailable")
	}
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		listener.Close()
		return inputBridge{}, err
	}
	ticket := hex.EncodeToString(bytes)
	endpoint := "http://" + listener.Addr().String() + "/" + ticket
	lifetime, cancel := context.WithCancel(ctx)
	failures := &upstreamFailureState{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) != 2 || subtle.ConstantTimeCompare([]byte(parts[0]), []byte(ticket)) != 1 || (r.Method != "GET" && r.Method != "HEAD") {
			http.NotFound(w, r)
			return
		}
		target, headers := source.URL, source.Headers
		if parts[1] == "audio" && source.AudioURL != "" {
			target, headers = source.AudioURL, source.AudioHeaders
		} else if parts[1] != "video" {
			http.NotFound(w, r)
			return
		}
		requestContext, stop := context.WithCancel(r.Context())
		defer stop()
		unlink := context.AfterFunc(lifetime, stop)
		defer unlink()
		if source.Item.Provider == "youtube" && YouTubeRangeOrigin(target) {
			if _, ok := openRangeStart(r.Header.Get("Range")); ok {
				started, err := relayYouTubeOpenRange(w, r.WithContext(requestContext), t.http, target, headers)
				if err != nil {
					if requestContext.Err() == nil {
						failures.record(upstreamErrorClass(err, ConversionFailureUpstreamRange), 0)
					}
					if started {
						panic(http.ErrAbortHandler)
					}
					http.Error(w, "unavailable", 502)
				}
				return
			}
		}
		request, err := http.NewRequestWithContext(requestContext, r.Method, target, nil)
		if err != nil {
			if requestContext.Err() == nil {
				failures.record(ConversionFailureUpstreamTransport, 0)
			}
			http.Error(w, "unavailable", 502)
			return
		}
		request.Header = headers.Clone()
		if request.Header == nil {
			request.Header = make(http.Header)
		}
		if value := r.Header.Get("Range"); value != "" {
			request.Header.Set("Range", value)
		}
		request.Header.Set("Accept-Encoding", "identity")
		response, err := t.http.Do(request)
		if err != nil {
			if requestContext.Err() == nil {
				failures.record(upstreamErrorClass(err, ConversionFailureUpstreamTransport), 0)
			}
			http.Error(w, "unavailable", 502)
			return
		}
		defer response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 206 && response.StatusCode != 416 {
			if requestContext.Err() == nil {
				failures.record(ConversionFailureUpstreamHTTP, response.StatusCode)
			}
			http.Error(w, "unavailable", 502)
			return
		}
		for _, header := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
			if value := response.Header.Get(header); value != "" {
				w.Header().Set(header, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		if r.Method != "HEAD" {
			if err := copyUpstreamBody(w, response.Body); err != nil && requestContext.Err() == nil {
				failures.record(upstreamErrorClass(err, ConversionFailureUpstreamTransport), 0)
			}
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	go server.Serve(listener)
	close := func() {
		cancel()
		_ = server.Close()
	}
	bridge := inputBridge{video: endpoint + "/video", close: close, failures: failures}
	if source.AudioURL != "" {
		bridge.audio = endpoint + "/audio"
	}
	return bridge, nil
}

// Manifest input uses a rewritten bounded resource graph; progressive input keeps two finite routes.
// ManifestKind identifies declared manifests without enabling recursive demuxers for opaque input.
func ManifestKind(source domain.Source) string {
	mime := strings.ToLower(source.MIME)
	parsed, _ := url.Parse(source.URL)
	path := ""
	if parsed != nil {
		path = strings.ToLower(parsed.Path)
	}
	if strings.Contains(mime, "mpegurl") || strings.HasSuffix(path, ".m3u8") {
		return "hls"
	}
	if strings.Contains(mime, "dash") || strings.HasSuffix(path, ".mpd") {
		return "dash"
	}
	return ""
}
func RemoteCandidate(source domain.Source) bool {
	if source.Path != "" {
		return false
	}
	kind := ManifestKind(source)
	if kind != "" && source.AudioURL != "" {
		return false
	}
	if source.Live && kind == "" {
		mime := strings.ToLower(strings.TrimSpace(strings.SplitN(source.MIME, ";", 2)[0]))
		if source.AudioURL != "" || (mime != "video/mp2t" && !IsLiveMP3Source(source)) {
			return false
		}
	}
	for _, raw := range []string{source.URL, source.AudioURL} {
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return false
		}
	}
	return source.URL != ""
}

// IsLiveMP3Source identifies an opaque live MPEG audio input eligible for
// capability-backed conversion; manifests and split audio inputs are excluded.
func IsLiveMP3Source(source domain.Source) bool {
	if !source.Live || source.AudioURL != "" || ManifestKind(source) != "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(strings.SplitN(source.MIME, ";", 2)[0]), "audio/mpeg")
}
