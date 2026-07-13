package github

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"sync"
)

const maxConditionalBodyBytes = 1 << 20

type conditionalCacheEntry struct {
	etag string
	body []byte
}

var conditionalRequests = struct {
	sync.RWMutex
	enabled bool
	entries map[string]conditionalCacheEntry
}{
	enabled: true,
	entries: map[string]conditionalCacheEntry{},
}

// ConditionalRequestStats describes the polling reads made by one GhClient.
// Calls counts ETag-capable requests; Misses also includes every other REST GET.
type ConditionalRequestStats struct {
	Calls  int
	Misses int
}

// ConfigureConditional enables or disables ETag conditional polling reads for
// the process. The response cache is process-global because daemon polling
// constructs a fresh GhClient for every repository tick.
func ConfigureConditional(enabled bool) {
	conditionalRequests.Lock()
	conditionalRequests.enabled = enabled
	if !enabled {
		conditionalRequests.entries = map[string]conditionalCacheEntry{}
	}
	conditionalRequests.Unlock()
}

func conditionalEnabled() bool {
	conditionalRequests.RLock()
	defer conditionalRequests.RUnlock()
	return conditionalRequests.enabled
}

func conditionalRequestKey(repo Repository, args []string) string {
	host := strings.TrimSpace(os.Getenv("GH_HOST"))
	if host == "" {
		host = "github.com"
	}
	// Length-prefix each literal arg so distinct argv lists cannot collide even
	// when values contain spaces or separators.
	var fingerprint strings.Builder
	for _, arg := range args {
		fingerprint.WriteString(strconv.Itoa(len(arg)))
		fingerprint.WriteByte(':')
		fingerprint.WriteString(arg)
	}
	return strings.ToLower(host) + "|" + repo.FullName() + "|" + fingerprint.String()
}

func loadConditionalEntry(key string) (conditionalCacheEntry, bool) {
	conditionalRequests.RLock()
	entry, ok := conditionalRequests.entries[key]
	conditionalRequests.RUnlock()
	if ok {
		entry.body = append([]byte(nil), entry.body...)
	}
	return entry, ok
}

func storeConditionalEntry(key, etag string, body []byte) {
	conditionalRequests.Lock()
	defer conditionalRequests.Unlock()
	if etag == "" || len(body) > maxConditionalBodyBytes {
		delete(conditionalRequests.entries, key)
		return
	}
	conditionalRequests.entries[key] = conditionalCacheEntry{
		etag: etag,
		body: append([]byte(nil), body...),
	}
}

func evictConditionalEntry(key string) {
	conditionalRequests.Lock()
	delete(conditionalRequests.entries, key)
	conditionalRequests.Unlock()
}

// resetConditionalForTest restores a deterministic cold, enabled cache.
func resetConditionalForTest() {
	conditionalRequests.Lock()
	conditionalRequests.enabled = true
	conditionalRequests.entries = map[string]conditionalCacheEntry{}
	conditionalRequests.Unlock()
}

type conditionalResponse struct {
	status  int
	etag    string
	body    []byte
	headers bool
}

// parseConditionalResponse strips one or more leading HTTP status/header blocks.
// gh may emit HTTP/1.1 or HTTP/2(.0), CRLF or LF, and proxy/interim blocks before
// the final response. Header names are case-insensitive; ETag values are retained
// byte-for-byte apart from the header's optional surrounding whitespace.
func parseConditionalResponse(output []byte) conditionalResponse {
	rest := output
	parsed := conditionalResponse{body: output}
	for {
		lineEnd, _ := firstLineEnd(rest)
		if lineEnd < 0 {
			return parsed
		}
		status, ok := parseHTTPStatusLine(string(rest[:lineEnd]))
		if !ok {
			return parsed
		}
		blockEnd, blockSep := firstHeaderEnd(rest)
		if blockEnd < 0 {
			return parsed
		}
		block := rest[:blockEnd]
		etag := ""
		lines := strings.Split(strings.ReplaceAll(string(block), "\r\n", "\n"), "\n")
		for _, line := range lines[1:] {
			name, value, found := strings.Cut(line, ":")
			if found && strings.EqualFold(strings.TrimSpace(name), "etag") {
				etag = strings.TrimSpace(value)
			}
		}
		rest = rest[blockEnd+blockSep:]
		parsed = conditionalResponse{status: status, etag: etag, body: rest, headers: true}
		// Continue only when another complete HTTP block starts immediately.
		nextLineEnd, _ := firstLineEnd(rest)
		if nextLineEnd < 0 {
			return parsed
		}
		if _, ok := parseHTTPStatusLine(string(rest[:nextLineEnd])); !ok {
			return parsed
		}
	}
}

func firstLineEnd(data []byte) (int, int) {
	for i, b := range data {
		if b == '\n' {
			if i > 0 && data[i-1] == '\r' {
				return i - 1, 2
			}
			return i, 1
		}
	}
	return -1, 0
}

func firstHeaderEnd(data []byte) (int, int) {
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	lf := bytes.Index(data, []byte("\n\n"))
	if crlf >= 0 && (lf < 0 || crlf <= lf) {
		return crlf, 4
	}
	if lf >= 0 {
		return lf, 2
	}
	return -1, 0
}

func parseHTTPStatusLine(line string) (int, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || !strings.HasPrefix(strings.ToUpper(fields[0]), "HTTP/") {
		return 0, false
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil || status < 100 || status > 599 {
		return 0, false
	}
	return status, true
}
