package server

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestRewrittenHLSResourceResolvesAtSessionAndNestedLevels(t *testing.T) {
	s := testServer(t, nil, "")
	sess := &session{
		ctx: context.Background(), ticket: "private-ticket",
		resources: make(map[string]string),
	}
	const id = "session-a"
	playlist := "#EXTM3U\n#EXTINF:4,\nsegment.ts\n"
	root, err := s.rewritePlaylist(sess, id, "https://origin.example/root.m3u8", playlist, false)
	if err != nil {
		t.Fatal(err)
	}
	rootReference := strings.Split(strings.TrimSpace(root), "\n")[2]
	if got := resolveHLSResource(t, "/v1/streams/"+id+"?ticket=private-ticket", rootReference); !strings.HasPrefix(got, "/v1/streams/"+id+"/") {
		t.Fatalf("root playlist resource resolved incorrectly: %q", got)
	}
	nested, err := s.rewritePlaylist(sess, id, "https://origin.example/child.m3u8", playlist, true)
	if err != nil {
		t.Fatal(err)
	}
	nestedReference := strings.Split(strings.TrimSpace(nested), "\n")[2]
	if strings.HasPrefix(nestedReference, id+"/") {
		t.Fatalf("nested resource repeats session directory: %q", nestedReference)
	}
	if got := resolveHLSResource(t, "/v1/streams/"+id+"/child-key?ticket=private-ticket", nestedReference); !strings.HasPrefix(got, "/v1/streams/"+id+"/") {
		t.Fatalf("nested playlist resource resolved incorrectly: %q", got)
	}
}

func resolveHLSResource(t *testing.T, playlistURL, reference string) string {
	t.Helper()
	if strings.HasPrefix(reference, "/") {
		t.Fatalf("playlist resource must be basename-relative for legacy HLS: %q", reference)
	}
	base, err := url.Parse(playlistURL)
	if err != nil {
		t.Fatal(err)
	}
	part, err := url.Parse(reference)
	if err != nil {
		t.Fatal(err)
	}
	resolved := base.ResolveReference(part)
	if !strings.HasPrefix(resolved.Path, "/v1/streams/") {
		t.Fatalf("resource escaped the stream route: %q", resolved.String())
	}
	return resolved.String()
}
