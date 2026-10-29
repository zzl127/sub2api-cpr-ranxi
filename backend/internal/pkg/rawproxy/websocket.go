package rawproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// WebSocket relays message payloads unchanged. Handshake/control framing is
// negotiated independently on each hop; no JSON frame is reserialized.
func WebSocket(ctx context.Context, w http.ResponseWriter, r *http.Request, opt Options) error {
	target, err := upstreamURL(opt, r)
	if err != nil {
		return err
	}
	if target.Scheme == "https" {
		target.Scheme = "wss"
	} else {
		target.Scheme = "ws"
	}
	headers := opt.Headers
	if headers == nil {
		headers = r.Header
	}
	var subprotocols []string
	for _, value := range headers.Values("Sec-Websocket-Protocol") {
		for _, v := range strings.Split(value, ",") {
			if v = strings.TrimSpace(v); v != "" {
				subprotocols = append(subprotocols, v)
			}
		}
	}
	// Preserve the COMPLETE failed upgrade response, not the library's
	// diagnostic-body prefix. Never follow a redirect with upstream credentials.
	client := *opt.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var failed *http.Response
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, e := transport.RoundTrip(req)
		if e == nil && resp.StatusCode != http.StatusSwitchingProtocols {
			failed = resp
			copy := *resp
			copy.Body = io.NopCloser(strings.NewReader(""))
			return &copy, nil
		}
		return resp, e
	})
	upstream, handshake, err := websocket.Dial(ctx, target.String(), &websocket.DialOptions{
		HTTPClient: &client, HTTPHeader: RequestHeaders(headers, opt.Token),
		Subprotocols: subprotocols, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if failed != nil {
			defer func() { _ = failed.Body.Close() }()
			CopyResponseHeaders(w.Header(), failed.Header)
			w.WriteHeader(failed.StatusCode)
			_, copyErr := io.Copy(&flushWriter{writer: w}, failed.Body)
			return copyErr
		}
		return err
	}
	defer func() { _ = upstream.CloseNow() }()
	accepted := []string(nil)
	if p := upstream.Subprotocol(); p != "" {
		accepted = []string{p}
	}
	downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: accepted, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return err
	}
	defer func() { _ = downstream.CloseNow() }()
	upstream.SetReadLimit(32 << 20)
	downstream.SetReadLimit(32 << 20)
	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	requestModel := ""
	requestedTier, requestedEffort := "", ""
	sessionTier, sessionEffort := "", ""
	requestedAt := time.Now()
	seen := make(map[string]struct{})
	meter := NewMeter()
	var meterTurnStarted time.Time
	exits := make(chan error, 2)
	go func() {
		for {
			kind, payload, e := downstream.Read(relayCtx)
			if e != nil {
				exits <- e
				return
			}
			event := gjson.GetBytes(payload, "type").String()
			if event == "response.create" || event == "session.update" {
				if opt.BeforeTurn != nil {
					if e = opt.BeforeTurn(payload); e != nil {
						_ = downstream.Close(websocket.StatusPolicyViolation, "Gateway quota or access limit reached")
						exits <- e
						return
					}
				}
				mu.Lock()
				nextModel := gjson.GetBytes(payload, "model").String()
				if nextModel == "" {
					nextModel = gjson.GetBytes(payload, "response.model").String()
				}
				if nextModel == "" {
					nextModel = gjson.GetBytes(payload, "session.model").String()
				}
				if nextModel != "" {
					requestModel = nextModel
				}
				if event == "response.create" {
					requestedAt = time.Now()
				}
				// Read only the request's options. A response.created default
				// echo must not erase the Fast tier sent on this WS turn.
				tier := gjson.GetBytes(payload, "service_tier")
				if !tier.Exists() {
					tier = gjson.GetBytes(payload, "response.service_tier")
				}
				if !tier.Exists() {
					tier = gjson.GetBytes(payload, "session.service_tier")
				}
				effort := gjson.GetBytes(payload, "reasoning.effort")
				if !effort.Exists() {
					effort = gjson.GetBytes(payload, "response.reasoning.effort")
				}
				if !effort.Exists() {
					effort = gjson.GetBytes(payload, "session.reasoning.effort")
				}
				if event == "session.update" {
					if tier.Exists() {
						sessionTier = tier.String()
					}
					if effort.Exists() {
						sessionEffort = effort.String()
					}
				} else {
					requestedTier, requestedEffort = sessionTier, sessionEffort
					if tier.Exists() {
						requestedTier = tier.String()
					}
					if effort.Exists() {
						requestedEffort = effort.String()
					}
				}
				mu.Unlock()
			}
			if e = upstream.Write(relayCtx, kind, payload); e != nil {
				exits <- e
				return
			}
		}
	}()
	go func() {
		for {
			kind, payload, e := upstream.Read(relayCtx)
			if e != nil {
				exits <- e
				return
			}
			event := gjson.GetBytes(payload, "type").String()
			mu.Lock()
			if event == "response.created" || !meterTurnStarted.Equal(requestedAt) {
				meterTurnStarted = requestedAt
				meter = NewMeterAt(meterTurnStarted, opt.StartsFirstToken)
				meter.RequestedModel = requestModel
				meter.RequestedServiceTier, meter.RequestedReasoningEffort = requestedTier, requestedEffort
			}
			mu.Unlock()
			meter.Observe(payload)
			var completedSample *Sample
			var completedDuration time.Duration
			terminal := event == "response.completed" || event == "response.failed" || event == "response.incomplete"
			if terminal && meter.Found && opt.OnTurn != nil {
				id := meter.ResponseID
				if _, duplicate := seen[id]; id != "" && !duplicate {
					// Bound per-connection deduplication without evicting live IDs.
					if len(seen) >= 65536 {
						exits <- io.ErrShortBuffer
						return
					}
					seen[id] = struct{}{}
					sample := meter.Sample
					if handshake != nil {
						sample.UpstreamHeaders = handshake.Header.Clone()
					}
					mu.Lock()
					if sample.Model == "" {
						sample.Model = requestModel
					}
					completedDuration = time.Since(meterTurnStarted)
					mu.Unlock()
					completedSample = &sample
				}
			}
			e = downstream.Write(relayCtx, kind, payload)
			// Deliver the terminal payload before bookkeeping; a slow billing
			// store must not hold a completed response away from the caller.
			if completedSample != nil {
				opt.OnTurn(*completedSample, completedDuration)
			}
			if e != nil {
				exits <- e
				return
			}
		}
	}()
	err = <-exits
	var closeError websocket.CloseError
	if errors.As(err, &closeError) {
		_ = upstream.Close(closeError.Code, closeError.Reason)
		_ = downstream.Close(closeError.Code, closeError.Reason)
	}
	cancel()
	_ = upstream.CloseNow()
	_ = downstream.CloseNow()
	<-exits // all relay goroutines stop before caller releases concurrency slots
	return err
}
