package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type soloistStatusClient struct{ body string }

func (c soloistStatusClient) Do(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(c.body))}, nil
}

func TestSoloistProviderMapsOnlyDeclaredFreshPCMTransport(t *testing.T) {
	for _, test := range []struct {
		backend, format                string
		available                      bool
		wantPCM, wantSource, wantError bool
	}{
		{"soloist", "s16le", true, true, true, false},
		{"soloist", "s16le", false, true, false, false},
		{"soloist", "f32le", true, false, false, true},
		{"untrusted", "s16le", true, false, false, true},
		{"go-librespot", "", true, false, true, false},
	} {
		available := "false"
		if test.available {
			available = "true"
		}
		client := soloistStatusClient{body: `{"backend":"` + test.backend + `","pcm_format":"` + test.format + `","audio_available":` + available + `,"stopped":false,"track":{"name":"Synthetic","artist_names":[],"duration":5000,"position":100}}`}
		adapters := New(client, client)
		config := Config{URL: "http://private-worker.invalid", Token: "synthetic-token-01234567890123456789"}
		sources, err := adapters.Spotify(context.Background(), config)
		if (err != nil) != test.wantError {
			t.Fatalf("backend=%s format=%s: %v", test.backend, test.format, err)
		}
		if err != nil {
			continue
		}
		if len(sources) != 1 || sources[0].RawPCM != test.wantPCM {
			t.Fatal("wrong transport")
		}
		if !test.wantPCM && sources[0].MIME != "audio/mpeg" {
			t.Fatal("go-librespot transport changed")
		}
		source, _, err := adapters.Reception(context.Background(), "spotify", config)
		if err != nil || (source != nil) != test.wantSource {
			t.Fatalf("unqualified incoming source admitted: %v", err)
		}
	}
}
