package rawproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func options(server *httptest.Server) Options {
	client := server.Client()
	original, ok := client.Transport.(*http.Transport)
	if !ok {
		panic("httptest client must use http.Transport")
	}
	transport := original.Clone()
	transport.DisableCompression = true
	client.Transport = transport
	return Options{BaseURL: server.URL + "/v1", Token: "upstream-fixture", Client: client, DownstreamToken: "downstream-fixture"}
}

func TestTargetPreservesEscapingAndQuery(t *testing.T) {
	u, err := url.Parse("/v1/responses/a%2Fb?z=a%20b&x=%2f&x=2")
	require.NoError(t, err)
	result, err := Target("http://localhost:99/prefix/v1", u)
	require.NoError(t, err)
	require.Equal(t, "/prefix/v1/responses/a%2Fb", result.EscapedPath())
	require.Equal(t, "z=a%20b&x=%2f&x=2", result.RawQuery)
}

func TestTargetNeverUsesCallerAuthority(t *testing.T) {
	for _, raw := range []string{
		"https://other.example.invalid/v1/responses?x=1",
		"//other.example.invalid/v1/responses",
		"/%2f%2fother.example.invalid/v1/responses",
		"https://user:password@other.example.invalid/v1/responses",
	} {
		t.Run(raw, func(t *testing.T) {
			incoming, err := url.Parse(raw)
			require.NoError(t, err)
			target, err := Target("https://configured.example.invalid/prefix", incoming)
			require.NoError(t, err)
			require.Equal(t, "https", target.Scheme)
			require.Equal(t, "configured.example.invalid", target.Host)
			require.Nil(t, target.User)
		})
	}
}

func TestHTTPExactRequestAndResponse(t *testing.T) {
	body := []byte(" { \"model\" : \"opaque-model\", \"instructions\":\"do not replace\", \"tools\":[{\"future\":null}], \"stream\": true }\r\n")
	reply := []byte(": upstream comment\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_exact\",\"model\":\"opaque-model\",\"usage\":{\"input_tokens\":100,\"output_tokens\":21,\"input_tokens_details\":{\"cached_tokens\":17}}}}\r\n\r\ndata: [DONE]\r\n\r\n")
	var count atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		require.Equal(t, "q=a%20b&x=%2f&x=2", r.URL.RawQuery)
		got, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, got)
		require.Equal(t, "Bearer upstream-fixture", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("X-Api-Key"))
		require.Empty(t, r.Header.Get("Cookie"))
		require.Empty(t, r.Header.Get("X-Remove"))
		require.Equal(t, "caller-ua/1.0", r.UserAgent())
		require.Equal(t, "unchanged-session", r.Header.Get("Session_id"))
		require.Equal(t, "raw-version", r.Header.Get("Version"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("X-Cpr", "one")
		w.Header().Add("X-Cpr", "two")
		w.Header().Set("X-Request-Id", "cpr-request")
		w.Header().Set("Connection", "X-Internal-Hop")
		w.Header().Set("X-Internal-Hop", "remove")
		_, _ = w.Write(reply)
	}))
	defer upstream.Close()
	req := httptest.NewRequest("POST", "/v1/responses?q=a%20b&key=downstream-fixture&x=%2f&x=2", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer downstream-fixture")
	req.Header.Set("X-Api-Key", "downstream-fixture")
	req.Header.Set("Cookie", "private-website-cookie")
	req.Header.Set("Connection", "X-Remove")
	req.Header.Set("X-Remove", "not-end-to-end")
	req.Header.Set("User-Agent", "caller-ua/1.0")
	req.Header.Set("Session_id", "unchanged-session")
	req.Header.Set("Version", "raw-version")
	rec := httptest.NewRecorder()
	result, err := HTTP(context.Background(), rec, req, body, options(upstream))
	require.NoError(t, err)
	require.Equal(t, reply, rec.Body.Bytes())
	require.Equal(t, []string{"one", "two"}, rec.Header().Values("X-Cpr"))
	require.Empty(t, rec.Header().Get("X-Internal-Hop"))
	require.Equal(t, int64(1), count.Load())
	require.True(t, result.Found)
	require.Equal(t, Usage{Input: 100, Output: 21, CacheRead: 17}, result.Usage)
}

func TestErrorsRedirectsAndLargeBodiesAreUnchanged(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 502, 307} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			reply := []byte(strings.Repeat("unmodified upstream error\n", 220000))
			var count atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				w.Header().Set("Location", "https://must-not-follow.invalid/")
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(status)
				_, _ = w.Write(reply)
			}))
			defer up.Close()
			rec := httptest.NewRecorder()
			res, err := HTTP(context.Background(), rec, httptest.NewRequest("POST", "/v1/responses", nil), nil, options(up))
			require.NoError(t, err)
			require.Equal(t, status, rec.Code)
			require.Equal(t, reply, rec.Body.Bytes())
			require.False(t, res.Found)
			require.Equal(t, int64(1), count.Load())
		})
	}
}

func TestGzipWireBytesPreservedUsageObserved(t *testing.T) {
	raw := []byte(`{"id":"resp_gzip","model":"fixture","usage":{"input_tokens":12,"output_tokens":7}}`)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	encoded := append([]byte(nil), buf.Bytes()...)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(encoded)
	}))
	defer up.Close()
	rec := httptest.NewRecorder()
	result, err := HTTP(context.Background(), rec, httptest.NewRequest("POST", "/v1/responses", nil), nil, options(up))
	require.NoError(t, err)
	require.Equal(t, encoded, rec.Body.Bytes())
	require.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	require.Equal(t, 12, result.Usage.Input)
	require.Equal(t, 7, result.Usage.Output)
}

func TestAnthropicCumulativeAndDuplicateUsage(t *testing.T) {
	m := NewMeter()
	m.Observe([]byte(`{"type":"message_start","message":{"id":"m","model":"fixture","type":"message","usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":3,"output_tokens":0}}}`))
	delta := []byte(`{"type":"message_delta","usage":{"output_tokens":8}}`)
	m.Observe(delta)
	m.Observe(delta)
	require.Equal(t, Usage{Input: 18, Output: 8, CacheRead: 5, CacheCreation: 3}, m.Usage)
}

func TestChatUsageAndMissingUsage(t *testing.T) {
	m := NewMeter()
	m.Observe([]byte(`{"id":"chat1","model":"fixture","usage":{"prompt_tokens":25,"completion_tokens":6,"prompt_tokens_details":{"cached_tokens":7}}}`))
	require.True(t, m.Found)
	require.Equal(t, Usage{Input: 25, Output: 6, CacheRead: 7}, m.Usage)
	empty := NewMeter()
	empty.Observe([]byte(`{"error":{"message":"do not rewrite"}}`))
	require.False(t, empty.Found)
}

func TestWebSocketPayloadsAndMetering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	payload := []byte(" { \"type\":\"response.create\", \"model\":\"fixture\", \"tools\":[{\"unknown\":null}] } \n")
	created := []byte(`{"type":"response.created","response":{"id":"resp_ws","model":"fixture"}}`)
	done := []byte(" {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ws\",\"model\":\"fixture\",\"usage\":{\"input_tokens\":9,\"output_tokens\":4}}} ")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer upstream-fixture", r.Header.Get("Authorization"))
		require.Equal(t, "caller-ws", r.Header.Get("Session_id"))
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		kind, got, err := conn.Read(ctx)
		if err != nil {
			return
		}
		require.Equal(t, websocket.MessageText, kind)
		require.Equal(t, payload, got)
		_ = conn.Write(ctx, websocket.MessageText, created)
		_ = conn.Write(ctx, websocket.MessageText, done)
		_ = conn.Write(ctx, websocket.MessageText, done)
		_, _, _ = conn.Read(ctx)
	}))
	defer up.Close()
	var bills, admissions atomic.Int64
	opt := options(up)
	opt.OnTurn = func(s Sample, d time.Duration) {
		require.Equal(t, Usage{Input: 9, Output: 4}, s.Usage)
		bills.Add(1)
	}
	opt.BeforeTurn = func([]byte) error { admissions.Add(1); return nil }
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = WebSocket(r.Context(), w, r, opt)
	}))
	defer proxy.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"Session_id": []string{"caller-ws"}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, payload))
	for _, want := range [][]byte{created, done, done} {
		_, got, readErr := conn.Read(ctx)
		require.NoError(t, readErr)
		require.Equal(t, want, got)
	}
	require.Equal(t, int64(1), bills.Load())
	require.Equal(t, int64(1), admissions.Load())
	_ = conn.Close(websocket.StatusNormalClosure, "finished")
}

func TestWebSocketUpgradeErrorBodyNotTruncated(t *testing.T) {
	reply := strings.Repeat("full-upstream-upgrade-error", 300)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, reply)
	}))
	defer up.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = WebSocket(r.Context(), w, r, options(up))
	}))
	defer proxy.Close()
	req, err := http.NewRequest("GET", proxy.URL+"/v1/responses", nil)
	require.NoError(t, err)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-Websocket-Version", "13")
	req.Header.Set("Sec-Websocket-Key", base64.StdEncoding.EncodeToString(make([]byte, 16)))
	resp, err := proxy.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 403, resp.StatusCode)
	require.Equal(t, reply, string(body))
}
