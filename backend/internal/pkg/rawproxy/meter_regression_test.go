package rawproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestHTTPFirstTokenIncludesHeaderWaitAndDoesNotBufferWire(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "cpr-fixture-id")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(30 * time.Millisecond)
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"early\"}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer up.Close()
	results := make(chan *Result, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result, _ := HTTP(r.Context(), w, r, nil, options(up))
		results <- result
	}))
	defer proxy.Close()
	start := time.Now()
	resp, err := proxy.Client().Get(proxy.URL + "/v1/responses")
	require.NoError(t, err)
	defer resp.Body.Close()
	buf := make([]byte, 256)
	n, err := resp.Body.Read(buf)
	require.NoError(t, err)
	require.Contains(t, string(buf[:n]), "early")
	select {
	case <-results:
		t.Fatal("proxy buffered the whole stream before forwarding the delta")
	default:
	}
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	result := <-results
	require.NotNil(t, result.FirstToken)
	require.GreaterOrEqual(t, *result.FirstToken, 170*time.Millisecond)
	require.Less(t, *result.FirstToken, result.Duration)
	require.Less(t, *result.FirstToken, time.Since(start))
	require.Equal(t, "cpr-fixture-id", result.Sample.UpstreamHeaders.Get("X-Request-Id"))
}

func TestMeterRequiresBothTokenTotals(t *testing.T) {
	m := NewMeter()
	m.Observe([]byte(`{"usage":{"input_tokens":5,"output_tokens":-1}}`))
	require.False(t, m.Found)
	m.Observe([]byte(`{"usage":{"output_tokens":0}}`))
	require.True(t, m.Found, "an explicit zero is different from a missing output total")
}

func TestMeterObservesHostedImageUsage(t *testing.T) {
	m := NewMeter()
	m.Observe([]byte(`{"response":{"usage":{"input_tokens":10,"output_tokens":20},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":8},"output_tokens_details":{"image_tokens":15}}}}}`))
	require.Equal(t, 8, m.Usage.ImageInput)
	require.Equal(t, 15, m.Usage.ImageOutput)
	require.Equal(t, 10, m.Usage.Input)
	require.Equal(t, 20, m.Usage.Output)
}

func TestWebSocketTurnMeterRetainsTierAndStartsBeforeCreated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for turn := 0; turn < 2; turn++ {
			_, _, err := conn.Read(ctx)
			if err != nil {
				return
			}
			time.Sleep(120 * time.Millisecond)
			// Turn two intentionally omits response.created to verify that
			// metadata/reset is tied to the REQUEST, not a guaranteed preamble.
			if turn == 0 {
				_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"r0","model":"observed-model","service_tier":"default"}}`))
			}
			time.Sleep(30 * time.Millisecond)
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"ok"}`))
			done := fmt.Sprintf(`{"type":"response.completed","response":{"id":"r%d","model":"observed-model","service_tier":"default","usage":{"input_tokens":12,"output_tokens":3}}}`, turn)
			_ = conn.Write(ctx, websocket.MessageText, []byte(done))
		}
		_, _, _ = conn.Read(ctx)
	}))
	defer up.Close()
	samples := make(chan Sample, 2)
	opt := options(up)
	opt.OnTurn = func(s Sample, _ time.Duration) { samples <- s }
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = WebSocket(r.Context(), w, r, opt)
	}))
	defer proxy.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	for turn, body := range []string{
		`{"type":"response.create","model":"caller-model","service_tier":"priority","reasoning":{"effort":"high"}}`,
		`{"type":"response.create","model":"caller-model"}`,
	} {
		require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(body)))
		for {
			_, payload, err := conn.Read(ctx)
			require.NoError(t, err)
			if gjson.GetBytes(payload, "type").String() == "response.completed" {
				break
			}
		}
		s := <-samples
		require.NotNil(t, s.FirstToken)
		require.GreaterOrEqual(t, *s.FirstToken, 140*time.Millisecond)
		require.Equal(t, "caller-model", s.RequestedModel)
		require.Equal(t, "observed-model", s.Model)
		require.Equal(t, "default", s.ServiceTier)
		if turn == 0 {
			require.Equal(t, "priority", s.RequestedServiceTier)
			require.Equal(t, "high", s.RequestedReasoningEffort)
		} else {
			require.Empty(t, s.RequestedServiceTier, "a prior turn must not charge Fast to a Standard turn")
			require.Empty(t, s.RequestedReasoningEffort)
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestMeterToolOnlyAndUsageOnlyEvents(t *testing.T) {
	m := NewMeter()
	m.Observe([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	require.Nil(t, m.FirstToken)
	m.Observe([]byte(`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"x\":1}"}}]}}]}`))
	require.NotNil(t, m.FirstToken)
}

func TestMeterUsesSSEEventFieldWithoutRewritingWire(t *testing.T) {
	m := NewMeterAt(time.Now(), func(body []byte) bool {
		return gjson.GetBytes(body, "type").String() == "response.output_item.added"
	})
	err := m.ObserveReader(strings.NewReader("event: response.output_item.added\ndata: {\"item\":{\"type\":\"reasoning\"}}\n\n"+
		"event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":7}}}\n\n"), true)
	require.NoError(t, err)
	require.NotNil(t, m.FirstToken)
	require.Equal(t, "response.completed", m.Event)
	require.Equal(t, 12, m.Usage.Input)
}

func TestMeterExplicitlyFlagsOversizedObservation(t *testing.T) {
	m := NewMeter()
	body := `{"output":"` + strings.Repeat("x", maxObservationBytes) + `","usage":{"input_tokens":12}}`
	require.NoError(t, m.ObserveReader(strings.NewReader(body), false))
	require.True(t, m.ObservationLimited)
	require.False(t, m.Found)
}

func TestMeterDoesNotInventCompletionOnEOF(t *testing.T) {
	m := NewMeter()
	require.NoError(t, m.ObserveReader(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"), true))
	require.False(t, m.Terminal)
	require.NoError(t, m.ObserveReader(strings.NewReader("data: [DONE]\n\n"), true))
	require.True(t, m.Terminal)
}
