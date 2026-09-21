package manifest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRejectsEscapingManifests(t *testing.T) {
	for _, fixture := range []struct{ kind, body string }{
		{"hls", "#EXTM3U\n#EXTINF:1,\nfile:///etc/passwd\n"},
		{"hls", "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://keys.test/key\"\n"},
		{"dash", `<MPD type="dynamic"><Period><AdaptationSet><Representation><SegmentList startNumber="5"><SegmentURL media="segment.m4s"/></SegmentList></Representation></AdaptationSet></Period></MPD>`},
		{"dash", `<MPD><Period><AdaptationSet><Representation><baseurl>file:///etc/passwd</baseurl></Representation></AdaptationSet></Period></MPD>`},
		{"dash", `<!DOCTYPE MPD [<!ENTITY x SYSTEM "file:///etc/passwd">]><MPD>&x;</MPD>`},
		{"dash", `<MPD><Period><AdaptationSet><ContentProtection schemeIdUri="urn:test"/></AdaptationSet></Period></MPD>`},
		{"dash", `<MPD xmlns:xlink="http://www.w3.org/1999/xlink"><Period xlink:href="https://outside.test/period"/></MPD>`},
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, fixture.body) }))
		proxy, err := Open(context.Background(), upstream.Client(), upstream.URL, fixture.kind, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.Get(proxy.URL)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 502 {
			t.Fatal("accepted unsafe manifest", fixture.kind, res.StatusCode)
		}
		proxy.Close()
		upstream.Close()
	}
}
func TestRewritesTemplatesAndInheritsBaseAndSegmentTemplate(t *testing.T) {
	p, err := Open(context.Background(), nil, "https://source.test/root/index.mpd", "dash", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	out, err := p.dash("https://source.test/root/index.mpd", []byte(`<MPD><BaseURL>../media/</BaseURL><Period><AdaptationSet><SegmentTemplate initialization="init-$RepresentationID$.mp4" media="part-$RepresentationID$-$Number%05d$.m4s"/><Representation id="v" bandwidth="123"/></AdaptationSet></Period></MPD>`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "https://source.test") || !strings.Contains(string(out), "$Number%05d$") {
		t.Fatal(string(out))
	}
	found := false
	for _, r := range p.entries {
		if r.url == "https://source.test/media/part-v-$Number%05d$.m4s" {
			found = true
		}
	}
	if !found {
		t.Fatal("lost inherited template", p.entries)
	}
}

func TestCrossOriginResourcesNeverReceiveProviderCredentials(t *testing.T) {
	var authenticated, segment bool
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credential escaped origin")
		}
		segment = true
		io.WriteString(w, "binary-segment")
	}))
	defer secondary.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated = r.Header.Get("Authorization") == "Bearer fixture"
		io.WriteString(w, "#EXTM3U\n#EXTINF:1,\n"+secondary.URL+"/segment.ts\n")
	}))
	defer upstream.Close()
	p, err := Open(t.Context(), upstream.Client(), upstream.URL, "hls", http.Header{"Authorization": {"Bearer fixture"}, "Cookie": {"secret=fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	res, err := http.Get(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	res, err = http.Get(lines[len(lines)-1])
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if !authenticated || !segment {
		t.Fatal("resources not fetched")
	}
}

func TestHLSVariantWithInterveningCommentStaysAManifest(t *testing.T) {
	p, err := Open(t.Context(), nil, "https://source.test/root.m3u8", "hls", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, err = p.hls("https://source.test/root.m3u8", []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\n# comment\nvariant.m3u8\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range p.entries {
		if strings.HasSuffix(entry.url, "variant.m3u8") && entry.kind == "hls" {
			return
		}
	}
	t.Fatal("variant treated as a segment")
}

func TestLiveRefreshRegistersNewSegmentsAndPreservesRange(t *testing.T) {
	count := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			count++
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nsegment"+fmt.Sprint(count)+".ts\n")
			return
		}
		if r.Header.Get("Range") != "bytes=2-5" {
			t.Error("range lost")
		}
		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(206)
		io.WriteString(w, "2345")
	}))
	defer upstream.Close()
	p, err := Open(t.Context(), upstream.Client(), upstream.URL+"/live.m3u8", "hls", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	previous := ""
	for i := 0; i < 2; i++ {
		res, err := http.Get(p.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		segment := lines[len(lines)-1]
		if segment == previous {
			t.Fatal("stale live playlist")
		}
		previous = segment
		req, _ := http.NewRequest("GET", segment, nil)
		req.Header.Set("Range", "bytes=2-5")
		res, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 206 || string(body) != "2345" || res.Header.Get("Content-Range") != "bytes 2-5/10" {
			t.Fatal("range response lost")
		}
	}
}

func TestSegmentCannotHideAnEncodedManifest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte{0xff, 0xfe, '<', 0, 'M', 0, 'P', 0, 'D', 0, '>', 0})
	}))
	defer upstream.Close()
	p, err := Open(t.Context(), upstream.Client(), upstream.URL, "hls", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	segment, err := p.register(upstream.URL, "segment", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(segment)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 502 {
		t.Fatal("accepted encoded nested manifest")
	}
}
