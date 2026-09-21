// Package manifest exposes a bounded, credential-isolated graph to media processes.
package manifest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Client interface {
	Do(*http.Request) (*http.Response, error)
}
type resource struct {
	url, kind string
	depth     int
	variables []string
}
type Proxy struct {
	URL     string
	mu      sync.Mutex
	client  Client
	root    *url.URL
	headers http.Header
	base    string
	entries map[string]resource
	order   []string
	slots   chan struct{}
	server  *http.Server
	cancel  context.CancelFunc
	ctx     context.Context
}

func Open(ctx context.Context, client Client, raw, kind string, headers http.Header) (*Proxy, error) {
	origin, err := absolute(raw)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 24)
	if _, err = rand.Read(nonce); err != nil {
		listener.Close()
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	p := &Proxy{client: client, root: origin, headers: headers.Clone(), entries: map[string]resource{}, slots: make(chan struct{}, 8), cancel: cancel, ctx: lifetime}
	p.base = "http://" + listener.Addr().String() + "/" + hex.EncodeToString(nonce) + "/"
	p.URL, err = p.register(raw, kind, 0, nil)
	if err != nil {
		listener.Close()
		cancel()
		return nil, err
	}
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	go p.server.Serve(listener)
	return p, nil
}

func (p *Proxy) Close() { p.cancel(); _ = p.server.Close() }

func absolute(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || u == nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Fragment != "" {
		return nil, errors.New("invalid manifest resource")
	}
	return u, nil
}

func resolve(base, raw string) (string, error) {
	b, err := absolute(base)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	result := b.ResolveReference(u).String()
	_, err = absolute(result)
	return result, err
}

func (p *Proxy) register(raw, kind string, depth int, variables []string) (string, error) {
	checked := raw
	for _, v := range variables {
		checked = strings.ReplaceAll(checked, v, "0")
	}
	if _, err := absolute(checked); err != nil || depth > 8 {
		return "", errors.New("manifest graph limit")
	}
	hash := sha256.Sum256([]byte(fmt.Sprint(depth) + kind + "\x00" + raw))
	id := hex.EncodeToString(hash[:16])
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.entries[id]; !exists {
		if len(p.order) >= 2048 {
			removed := false
			for i, key := range p.order {
				if p.entries[key].kind == "segment" {
					delete(p.entries, key)
					p.order = append(p.order[:i], p.order[i+1:]...)
					removed = true
					break
				}
			}
			if !removed {
				return "", errors.New("manifest graph full")
			}
		}
		p.entries[id] = resource{raw, kind, depth, variables}
		p.order = append(p.order, id)
	}
	suffix := "media.bin"
	if kind == "hls" {
		suffix = "index.m3u8"
	}
	if kind == "dash" {
		suffix = "index.mpd"
	}
	if kind == "segment" {
		suffix = "media.m4s"
	}
	path := p.base + id + "/"
	for _, variable := range variables {
		path += variable + "/"
	}
	return path + suffix, nil
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	prefix, _ := url.Parse(p.base)
	if !strings.HasPrefix(r.URL.Path, prefix.Path) || (r.Method != "GET" && r.Method != "HEAD") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix.Path), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	entry, ok := p.entries[parts[0]]
	p.mu.Unlock()
	if !ok || len(parts) != len(entry.variables)+2 {
		http.NotFound(w, r)
		return
	}
	target := entry.url
	for i, variable := range entry.variables {
		value := parts[i+1]
		if len(value) == 0 || len(value) > 20 || strings.Trim(value, "0123456789") != "" {
			http.NotFound(w, r)
			return
		}
		target = strings.ReplaceAll(target, variable, value)
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "busy", 429)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	unlink := context.AfterFunc(p.ctx, cancel)
	defer unlink()
	req, err := http.NewRequestWithContext(ctx, r.Method, target, nil)
	if err != nil {
		http.Error(w, "invalid resource", 502)
		return
	}
	if req.URL.Scheme == p.root.Scheme && req.URL.Host == p.root.Host {
		req.Header = p.headers.Clone()
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if value := r.Header.Get("Range"); value != "" && entry.kind == "segment" {
		req.Header.Set("Range", value)
	}
	req.Header.Set("Accept-Encoding", "identity")
	response, err := p.client.Do(req)
	if err != nil {
		http.Error(w, "unavailable", 502)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 && response.StatusCode != 206 {
		http.Error(w, "unavailable", 502)
		return
	}
	if r.Method == "HEAD" {
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if entry.kind == "segment" {
		reader := bufio.NewReader(response.Body)
		// Never let a disguised playlist make the demuxer fetch unrewritten URLs.
		first, _ := reader.Peek(512)
		trimmed := strings.TrimSpace(strings.TrimPrefix(string(first), "\ufeff"))
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "<") || bytes.HasPrefix(first, []byte{0xff, 0xfe}) || bytes.HasPrefix(first, []byte{0xfe, 0xff}) || bytes.HasPrefix(first, []byte{0, 0, 0xfe, 0xff}) {
			http.Error(w, "unexpected manifest", 502)
			return
		}
		for _, header := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
			if v := response.Header.Get(header); v != "" {
				w.Header().Set(header, v)
			}
		}
		w.WriteHeader(response.StatusCode)
		if _, err = io.CopyN(w, reader, 128<<20); err != io.EOF {
			panic(http.ErrAbortHandler)
		}
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		http.Error(w, "manifest limit", 502)
		return
	}
	base := target
	if response.Request != nil && response.Request.URL != nil {
		base = response.Request.URL.String()
	}
	var output []byte
	if entry.kind == "hls" {
		output, err = p.hls(base, body, entry.depth)
	} else {
		output, err = p.dash(base, body, entry.depth)
	}
	if err != nil {
		http.Error(w, "unsupported manifest", 502)
		return
	}
	if entry.kind == "hls" {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	} else {
		w.Header().Set("Content-Type", "application/dash+xml")
	}
	fmt.Fprint(w, string(output))
}
