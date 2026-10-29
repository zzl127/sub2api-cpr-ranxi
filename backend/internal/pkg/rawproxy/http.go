package rawproxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type Options struct {
	BaseURL string
	Token   string
	Client  *http.Client
	// Original headers captured before local request-ID/endpoint middleware.
	Headers         http.Header
	DownstreamToken string
	OnTurn          func(Sample, time.Duration)
	// WS quota admission. This callback never mutates the supplied frame.
	BeforeTurn       func([]byte) error
	StartsFirstToken func([]byte) bool
	// ObservePayload receives bounded decoded observations, never wire ownership.
	ObservePayload func([]byte)
}

// RequestTarget removes only this gateway's authentication query values.
func RequestTarget(opt Options, r *http.Request) (*url.URL, error) {
	return upstreamURL(opt, r)
}

func upstreamURL(opt Options, r *http.Request) (*url.URL, error) {
	incoming := *r.URL
	if opt.DownstreamToken != "" {
		parts := strings.Split(incoming.RawQuery, "&")
		kept := parts[:0]
		for _, part := range parts {
			pair := strings.SplitN(part, "=", 2)
			name, _ := url.QueryUnescape(pair[0])
			value := ""
			if len(pair) == 2 {
				value, _ = url.QueryUnescape(pair[1])
			}
			if (name == "api_key" || name == "key" || name == "access_token") && value == opt.DownstreamToken {
				continue
			}
			kept = append(kept, part)
		}
		incoming.RawQuery = strings.Join(kept, "&")
	}
	return Target(opt.BaseURL, &incoming)
}

type Result struct {
	Sample
	StatusCode       int
	Headers          http.Header
	Duration         time.Duration
	ClientDisconnect bool
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func StripHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			h.Del(strings.TrimSpace(token))
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// Target retains escaped paths and the original query, without path.Clean.
// A conventional base ending in /v1 represents the same inbound /v1 prefix.
func Target(base string, incoming *url.URL) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid full-passthrough base URL")
	}
	prefix := strings.TrimSuffix(strings.TrimSuffix(u.EscapedPath(), "/"), "/v1")
	raw := prefix + incoming.EscapedPath()
	if raw == "" {
		raw = "/"
	}
	path, err := url.PathUnescape(raw)
	if err != nil {
		return nil, errors.New("invalid escaped request path")
	}
	u.Path, u.RawPath = path, raw
	if u.RawQuery != "" && incoming.RawQuery != "" {
		u.RawQuery += "&" + incoming.RawQuery
	} else if incoming.RawQuery != "" {
		u.RawQuery = incoming.RawQuery
	}
	u.Fragment = ""
	return u, nil
}

func RequestHeaders(original http.Header, token string) http.Header {
	h := original.Clone()
	StripHopHeaders(h)
	for _, name := range []string{
		"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie", "Host", "Content-Length",
		"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
		"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol",
	} {
		h.Del(name)
	}
	h.Set("Authorization", "Bearer "+token)
	if _, exists := h["User-Agent"]; !exists {
		h["User-Agent"] = []string{""} // suppress Go's automatically inserted UA
	}
	return h
}

func CopyResponseHeaders(dst, src http.Header) {
	h := src.Clone()
	StripHopHeaders(h)
	h.Del("Set-Cookie") // never replace the Sub2API website's login cookies
	for name, values := range h {
		dst[name] = append([]string(nil), values...)
	}
	if _, exists := h["Content-Type"]; !exists {
		dst["Content-Type"] = nil // do not sniff/reclassify opaque content
	}
}

type flushWriter struct {
	writer http.ResponseWriter
	failed bool
}

func (w *flushWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err != nil {
		w.failed = true
		return n, err
	}
	if f, ok := w.writer.(http.Flusher); ok {
		f.Flush()
	}
	return n, nil
}

func HTTP(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, opt Options) (*Result, error) {
	start := time.Now()
	target, err := upstreamURL(opt, r)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	headers := opt.Headers
	if headers == nil {
		headers = r.Header
	}
	request.Header = RequestHeaders(headers, opt.Token)
	client := *opt.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	result := &Result{StatusCode: response.StatusCode, Headers: response.Header.Clone()}
	CopyResponseHeaders(w.Header(), response.Header)
	// Trailers are separate end-to-end fields. Never turn them into body events.
	for name := range response.Trailer {
		w.Header().Add("Trailer", name)
	}
	w.WriteHeader(response.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	sink := &flushWriter{writer: w}
	tee := io.TeeReader(response.Body, sink)
	// The origin is BEFORE dialing and waiting for response headers, not after.
	meter := NewMeterAt(start, opt.StartsFirstToken)
	meter.ObservePayload = opt.ObservePayload
	observer := io.Reader(tee)
	var closeDecoder func()
	encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding")))
	switch encoding {
	case "", "identity":
	case "gzip":
		var decoder *gzip.Reader
		decoder, err = gzip.NewReader(tee)
		if err == nil {
			observer, closeDecoder = decoder, func() { _ = decoder.Close() }
		}
	case "deflate":
		var decoder io.ReadCloser
		decoder, err = zlib.NewReader(tee)
		if err == nil {
			observer, closeDecoder = decoder, func() { _ = decoder.Close() }
		}
	case "br":
		observer = brotli.NewReader(tee)
	case "zstd":
		var decoder *zstd.Decoder
		decoder, err = zstd.NewReader(tee, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
		if err == nil {
			observer, closeDecoder = decoder, decoder.Close
		}
	default:
		err = errors.New("unsupported observation encoding")
	}
	compressed := encoding != "" && encoding != "identity"
	var observedLimit *io.LimitedReader
	if compressed {
		// Bound decompression work independently of raw forwarding.
		observedLimit = &io.LimitedReader{R: observer, N: 64 << 20}
		observer = observedLimit
	}
	if err == nil {
		err = meter.ObserveReader(observer, strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream"))
	} else {
		meter.ObservationLimited = true
		// Unknown/broken observation encoding does not change the transmitted bytes.
		_, err = io.Copy(io.Discard, tee)
	}
	if err != nil || (observedLimit != nil && observedLimit.N == 0) {
		meter.ObservationLimited = true
	}
	if closeDecoder != nil {
		closeDecoder()
	}
	if compressed && !sink.failed && ctx.Err() == nil {
		// Any unread raw bytes (including after an observation cap/error) still
		// reach the client. No decoded data is ever written to the client.
		_, err = io.Copy(io.Discard, tee)
	}
	for name, values := range response.Trailer {
		w.Header()[http.TrailerPrefix+name] = append([]string(nil), values...)
	}
	result.Sample, result.Duration = meter.Sample, time.Since(start)
	result.Sample.UpstreamHeaders = response.Header.Clone()
	result.ClientDisconnect = sink.failed || ctx.Err() != nil
	return result, err
}
