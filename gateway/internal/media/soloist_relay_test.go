package media

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

type soloistRelayClient struct {
	mime    string
	session string
}

func (c *soloistRelayClient) Do(r *http.Request) (*http.Response, error) {
	c.session = r.Header.Get("X-Zombie-Session")
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {c.mime}}, Body: io.NopCloser(bytes.NewReader([]byte{1, 0, 2, 0, 3, 0, 4, 0}))}, nil
}
func TestSoloistRawRelayRequiresExactMIMEAndScopesSession(t *testing.T) {
	c := &soloistRelayClient{mime: "audio/mpeg"}
	remote := NewRemote(nil, c)
	source := domain.Source{Item: domain.Item{Provider: "spotify"}, Live: true, RawPCM: true, PCMFormat: "s16le", URL: "http://worker.invalid/audio"}
	var output bytes.Buffer
	if err := remote.ConvertRemote(context.Background(), source, "RAW_PCM", domain.MediaSelection{}, &output); err == nil || output.Len() != 0 {
		t.Fatal("encoded input labeled PCM")
	}
	c.mime = PCMStreamMIME
	if err := remote.ConvertRemote(context.Background(), source, "RAW_PCM", domain.MediaSelection{}, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), []byte{1, 0, 2, 0, 3, 0, 4, 0}) || len(c.session) != 32 {
		t.Fatal("PCM changed or unscoped session")
	}
	source.PCMFormat = "f32le"
	if err := remote.ConvertRemote(context.Background(), source, "RAW_PCM", domain.MediaSelection{}, io.Discard); err == nil {
		t.Fatal("unsupported format admitted")
	}
}
