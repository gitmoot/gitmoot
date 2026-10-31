package credgw

import (
	"net/http"
	"strings"
)

// A source capability grants repository reads, never receive-pack, API writes,
// LFS uploads, arbitrary queries, or another repository beneath the same host.
func gitReadRequestAllowed(request *http.Request, suffix string) bool {
	switch {
	case request.Method == http.MethodGet && suffix == "/info/refs":
		query := request.URL.Query()
		return len(query) == 1 && len(query["service"]) == 1 && query.Get("service") == "git-upload-pack" && request.URL.RawQuery == "service=git-upload-pack"
	case request.Method == http.MethodPost && suffix == "/git-upload-pack":
		mediaType, _, _ := strings.Cut(request.Header.Get("Content-Type"), ";")
		return request.URL.RawQuery == "" && strings.TrimSpace(mediaType) == "application/x-git-upload-pack-request" && request.Header.Get("Content-Encoding") == ""
	default:
		return false
	}
}
