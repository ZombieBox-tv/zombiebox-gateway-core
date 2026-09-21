// Package config loads optional operator-owned provider settings. It never logs values.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"zombiebox.local/gateway/internal/providers"
)

func Load(path string, lookup func(string) (string, bool)) (map[string]providers.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open provider configuration")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, errors.New("configuration exceeds 64 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var configs map[string]providers.Config
	if dec.Decode(&configs) != nil || dec.Decode(new(any)) != io.EOF || configs == nil {
		return nil, errors.New("invalid provider configuration JSON")
	}
	for id, c := range configs {
		if _, ok := providers.Titles[id]; !ok || id == "local" {
			return nil, errors.New("unknown provider identifier")
		}
		// URLs can contain IPTV subscription keys, so they support env references too.
		for _, field := range []*string{&c.Token, &c.URL, &c.EPGURL, &c.UserID} {
			if strings.HasPrefix(*field, "env:") {
				value, ok := lookup(strings.TrimPrefix(*field, "env:"))
				if (!ok || value == "") && c.Enabled {
					return nil, errors.New("enabled provider references a missing environment variable")
				}
				*field = value
			}
		}
		if providers.Validate(c) != nil {
			return nil, errors.New("invalid provider fields")
		}
		configs[id] = c
	}
	return configs, nil
}
