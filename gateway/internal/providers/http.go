// Package providers isolates remote metadata and credentials from the client protocol.
package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

func (a *Adapters) request(ctx context.Context, raw string, headers http.Header) ([]byte, error) {
	return requestWithClient(ctx, a.http, raw, headers)
}

func requestWithClient(ctx context.Context, client HTTPClient, raw string, headers http.Header) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("provider unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("provider HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20+1))
	if len(body) > 8<<20 {
		return nil, errors.New("provider response too large")
	}
	return body, err
}
