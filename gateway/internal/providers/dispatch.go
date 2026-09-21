// Package providers isolates remote metadata and credentials from the client protocol.
package providers

import (
	"context"
	"errors"
)

func (a *Adapters) Fetch(ctx context.Context, id string, c Config, mediaDir string) ([]Source, error) {
	if id == "local" {
		return Local(mediaDir)
	}
	if !c.Enabled {
		return []Source{}, nil
	}
	if adapter, ok := a.catalog[id]; ok {
		return adapter.Fetch(ctx, c)
	}
	return nil, errors.New("adapter not implemented")
}
