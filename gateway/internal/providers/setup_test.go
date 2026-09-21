package providers

import (
	"net/http"
	"time"
)

var testAdapters = New(&http.Client{Timeout: 5 * time.Second, CheckRedirect: SafeRedirect}, &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
