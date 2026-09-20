package providers

import (
	"errors"
	"net/http"
)

// Allow bounded CDN redirects, stripping all sensitive provider headers when
// the origin changes. Never pass credentials through HTTPS -> HTTP downgrade.
func SafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 4 || req.URL.User != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return errors.New("unsupported provider redirect")
	}
	previous := via[len(via)-1].URL
	if previous.Scheme == "https" && req.URL.Scheme == "http" {
		return errors.New("insecure provider redirect")
	}
	// net/http can copy custom headers from the initial request on every hop.
	// Compare with that initial origin, including a second hop within a CDN.
	original := via[0].URL
	if original.Host != req.URL.Host || original.Scheme != req.URL.Scheme {
		header := http.Header{}
		for _, key := range []string{"Accept", "Range"} {
			if value := req.Header.Get(key); value != "" {
				header.Set(key, value)
			}
		}
		req.Header = header
	}
	return nil
}
