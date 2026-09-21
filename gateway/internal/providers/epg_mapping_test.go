package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEPGMappingPreservesChannelIdentityAndExtendsGuide(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	programme := func(hours int, title string) string {
		return fmt.Sprintf(`<programme channel="mapped" start="%s" stop="%s"><title>%s</title></programme>`, now.Add(time.Duration(hours)*time.Hour).Format("20060102150405 -0700"), now.Add(time.Duration(hours+2)*time.Hour).Format("20060102150405 -0700"), title)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/list" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:-1 tvg-id=\"original\",News\nhttps://example.invalid/live\n")
			return
		}
		fmt.Fprint(w, "<tv>"+programme(-1, "Now")+programme(36, "Tomorrow")+programme(49, "Outside")+"</tv>")
	}))
	defer server.Close()
	adapters := New(server.Client(), server.Client())
	config := Config{URL: server.URL + "/list", EPGURL: server.URL + "/guide"}
	original, err := adapters.IPTV(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	config.EPGMappings = map[string]string{"original": "mapped"}
	mapped, err := adapters.IPTV(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 1 || len(mapped) != 1 || original[0].Item.ID != mapped[0].Item.ID {
		t.Fatal("mapping changed channel identity")
	}
	item := mapped[0].Item
	if len(item.Programmes) != 2 || item.Subtitle != "Now" || item.Programmes[1].Title != "Tomorrow" {
		t.Fatalf("unexpected guide: %+v", item)
	}
}
