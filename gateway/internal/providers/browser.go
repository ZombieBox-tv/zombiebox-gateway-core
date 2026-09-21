package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// BrowserRequest is a private adapter call; paths are constructed by the gateway,
// never accepted from Android. The response cap also bounds legacy bitmap memory.
func BrowserRequest(ctx context.Context, c Config, method, path string, body any) ([]byte, error) {
	headers, err := wrapperHeaders(c)
	if err != nil {
		return nil, err
	}
	var input []byte
	if body != nil {
		input, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, bytes.NewReader(input))
	if err != nil {
		return nil, err
	}
	req.Header = headers
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("browser unavailable")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("invalid browser response")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, errors.New("browser unavailable")
	}
	return data, nil
}
