package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"llm_api_gateway/internal/provider"
)

// buildPassthroughTarget assembles the upstream URL from the provider's base
// endpoint and the captured sub-path + raw query. The endpoint is treated as a
// base URL; the sub-path is appended verbatim (it already carries a leading
// slash). See design Q1 — passthrough uses the provider endpoint as a base
// and does NOT append /chat/completions (unlike the chat handler).
func buildPassthroughTarget(endpoint, subPath, rawQuery string) string {
	base := strings.TrimSuffix(endpoint, "/")
	target := base + subPath
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	return target
}

// buildUpstreamRequest constructs the upstream HTTP request for a passthrough
// call. It:
//   - uses the client's original method and the precomputed target URL;
//   - copies client request headers EXCEPT Host / client auth / hop-by-hop;
//   - clears req.Host so Go rewrites it from the target URL (design Q7);
//   - injects the real upstream auth via injectUpstreamAuth (hides sub-key).
func buildUpstreamRequest(r *http.Request, targetURL string, bodyBytes []byte, prov provider.ProviderEntry) (*http.Request, error) {
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	} else {
		bodyReader = http.NoBody
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bodyReader)
	if err != nil {
		return nil, err
	}

	// Copy client headers (excluding sensitive / hop-by-hop / Host).
	for key, values := range r.Header {
		lkey := strings.ToLower(key)
		if lkey == "host" || lkey == "authorization" ||
			lkey == "x-api-key" || lkey == "proxy-authorization" {
			continue
		}
		if isHopByHop(key) {
			continue
		}
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
	// Let Go set Host from the target URL; do NOT copy the client Host
	// (would otherwise be rejected by some upstreams as a 400).
	req.Host = ""

	injectUpstreamAuth(req, prov)
	return req, nil
}

// injectUpstreamAuth injects the provider's real upstream credential according
// to its auth scheme, and applies any static extra headers. The client sub-key
// has already been stripped by buildUpstreamRequest. The real key is never
// written to logs (design §8 / ADR-0002 / ADR-0007).
func injectUpstreamAuth(req *http.Request, prov provider.ProviderEntry) {
	scheme := prov.AuthScheme
	if scheme == "" {
		scheme = "bearer"
	}

	switch scheme {
	case "x-api-key":
		header := prov.AuthHeader
		if header == "" {
			header = "X-Api-Key"
		}
		req.Header.Set(header, prov.APIKey)
	case "none":
		// Inject nothing unless an explicit auth header was configured;
		// rely on extra_headers / pre-shared upstream trust instead.
		if prov.AuthHeader != "" {
			req.Header.Set(prov.AuthHeader, prov.APIKey)
		}
	default: // "bearer" and any unknown scheme fall back to bearer
		header := prov.AuthHeader
		if header == "" {
			header = "Authorization"
		}
		req.Header.Set(header, "Bearer "+prov.APIKey)
	}

	// Static extra headers (e.g. anthropic-version) are injected verbatim.
	for k, v := range prov.ExtraHeaders {
		req.Header.Set(k, v)
	}
}

// copyResponseHeaders forwards upstream response headers to the client, stripping
// hop-by-hop headers and (for streaming) Content-Length. For streaming it also
// sets X-Accel-Buffering: no so nginx does not buffer the SSE/chunked body.
func copyResponseHeaders(dst http.ResponseWriter, src http.Header, isStream bool) {
	for key, values := range src {
		if isHopByHop(key) {
			continue
		}
		for _, v := range values {
			dst.Header().Add(key, v)
		}
	}
	if isStream {
		dst.Header().Del("Content-Length")
		dst.Header().Set("X-Accel-Buffering", "no")
	}
}

// isHopByHop reports whether a header is connection-specific and must not be
// forwarded. Comparison is case-insensitive (RFC 7230 §6.1).
func isHopByHop(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailers",
		"transfer-encoding", "upgrade":
		return true
	}
	return false
}

// streamCopy forwards the upstream response body to the client, flushing after
// each chunk so SSE / chunked responses arrive live. It aborts immediately if
// the client disconnects (r.Context().Done()) so we free the upstream
// connection and release the per-user concurrency slot. The JSON body is never
// inspected or logged.
func streamCopy(w http.ResponseWriter, r *http.Request, body io.ReadCloser) error {
	defer body.Close()

	flusher, ok := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		default:
		}

		n, readErr := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if ok {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
