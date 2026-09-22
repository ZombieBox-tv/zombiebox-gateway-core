package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

func (a *Adapters) receiverCall(ctx context.Context, c Config, method, path string, input, output any) error {
	headers, err := wrapperHeaders(c)
	if err != nil {
		return err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header = headers
	req.Header.Set("Content-Type", "application/json")
	res, err := a.privateHTTP.Do(req)
	if err != nil {
		return errors.New("receiver unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New("receiver rejected request")
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16<<10+1))
	if err != nil || len(raw) > 16<<10 {
		return errors.New("invalid receiver response")
	}
	if output != nil {
		return json.Unmarshal(raw, output)
	}
	return nil
}
func (a *Adapters) OpenReceiver(ctx context.Context, c Config, id string) (domain.YouTubeReceiverState, error) {
	var out domain.YouTubeReceiverState
	err := a.receiverCall(ctx, c, "POST", "/receiver", map[string]string{"receiverId": id}, &out)
	return out, err
}
func (a *Adapters) PollReceiver(ctx context.Context, c Config, id string) (domain.YouTubeReceiverState, error) {
	var out domain.YouTubeReceiverState
	err := a.receiverCall(ctx, c, "GET", "/receiver/"+id, nil, &out)
	return out, err
}
func (a *Adapters) AcknowledgeReceiver(ctx context.Context, c Config, id string, state domain.ReceiverAcknowledgement) error {
	return a.receiverCall(ctx, c, "POST", "/receiver/"+id+"/state", state, nil)
}
func (a *Adapters) CloseReceiver(ctx context.Context, c Config, id string) error {
	return a.receiverCall(ctx, c, "DELETE", "/receiver/"+id, nil, nil)
}

func (a *Adapters) SuspendReceiver(ctx context.Context, c Config, id, epoch string) error {
	return a.receiverCall(ctx, c, "POST", "/receiver/"+id+"/suspend", map[string]string{"epoch": epoch}, nil)
}
