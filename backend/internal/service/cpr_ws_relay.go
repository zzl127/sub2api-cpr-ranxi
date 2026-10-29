package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/rawproxy"
	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const CPRFirstWSFrameKey = "cpr_original_first_ws_frame"

type cprFrameSide struct {
	conn     *coderws.Conn
	read     func(context.Context) (coderws.MessageType, []byte, error)
	filter   func([]byte) error
	received atomic.Pointer[coderws.CloseError]
	peer     *cprFrameSide
}

func (s *cprFrameSide) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	kind, body, err := s.read(ctx)
	if err != nil {
		var closeErr coderws.CloseError
		if errors.As(err, &closeErr) {
			s.received.Store(&closeErr)
		}
		return kind, body, err
	}
	if s.filter != nil {
		err = s.filter(body)
	}
	return kind, body, err
}

func (s *cprFrameSide) WriteFrame(ctx context.Context, kind coderws.MessageType, body []byte) error {
	return s.conn.Write(ctx, kind, body)
}

func (s *cprFrameSide) Close() error {
	if s.peer != nil {
		if received := s.peer.received.Load(); received != nil {
			_ = s.conn.Close(received.Code, received.Reason)
		}
	}
	return s.conn.CloseNow()
}

// CPR frames remain opaque on the wire. Ranxi's existing relay measures TTFT
// and its ingress hooks own authorization, admission, pricing and slot release.
func (s *OpenAIGatewayService) proxyCPRWebSocket(ctx context.Context, c *gin.Context, downstream *coderws.Conn, a *Account, first []byte, hooks *OpenAIWSIngressHooks) error {
	if hooks == nil {
		return errors.New("CPR websocket requires native ingress hooks")
	}
	if original, ok := c.Get(CPRFirstWSFrameKey); ok {
		if frame, valid := original.([]byte); valid {
			first = frame
		}
	}
	if cprInvalidJSONKeys(first) {
		return errors.Join(ErrCPRRelay, NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "invalid or duplicate JSON keys", nil))
	}
	options, cleanup, err := s.cprRelayOptions(ctx, a)
	defer cleanup()
	if err != nil {
		return err
	}
	origin := c.Request.URL
	headers := c.Request.Header
	if captured := httputil.OriginalWireRequest(c.Request); captured != nil {
		origin = &captured.URL
		headers = captured.Header
	}
	targetRequest := c.Request.Clone(ctx)
	targetRequest.URL = origin
	if key := getAPIKeyFromContext(c); key != nil {
		options.DownstreamToken = key.Key
	}
	target, err := rawproxy.RequestTarget(options, targetRequest)
	if err != nil {
		return err
	}
	if target.Scheme == "https" {
		target.Scheme = "wss"
	} else {
		target.Scheme = "ws"
	}
	options.Client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dialCtx, cancelDial := context.WithTimeout(ctx, s.openAIWSDialTimeout())
	upstream, handshake, err := coderws.Dial(dialCtx, target.String(), &coderws.DialOptions{
		HTTPClient: options.Client, HTTPHeader: rawproxy.RequestHeaders(headers, a.GetCPRClientKey()),
		CompressionMode: coderws.CompressionDisabled,
	})
	cancelDial()
	if err != nil {
		return ErrCPRRelay
	}
	upstream.SetReadLimit(ResolveOpenAIWSClientReadLimitBytes(s.cfg))
	reader := openAIWSGetIngressReader(c, downstream)
	client := &cprFrameSide{conn: downstream, read: reader.read}
	server := &cprFrameSide{conn: upstream, read: upstream.Read, peer: client}
	client.peer = server
	defer client.Close()
	defer server.Close()

	meta := newOpenAIWSPassthroughUsageMeta(hooks.InitialRequestModel, first)
	meta.initFromFirstFrame(first, meta.requestModelForFrame(first))
	meta.captureRequestedReasoningEffort(first, hooks.InitialRequestModel)
	var imageBilling atomic.Pointer[OpenAIResponsesImageBillingConfig]
	initialImageBilling, _ := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(first, hooks.InitialRequestModel)
	imageBilling.Store(&initialImageBilling)
	if hooks.MapRequestModel != nil {
		if _, err := hooks.MapRequestModel(1, meta.requestModelForFrame(first)); err != nil {
			return err
		}
	}
	var accepted, settled atomic.Int32
	accepted.Store(1)
	start := hooks.InitialTurnStartedAt
	if start.IsZero() {
		start = time.Now()
	}
	var currentStart atomic.Int64
	currentStart.Store(start.UnixNano())
	var nextStart atomic.Pointer[time.Time]
	lifecycle := newOpenAIWSPassthroughTurnLifecycle(true)
	var admissionMu sync.Mutex
	closed := false
	client.filter = func(frame []byte) error {
		admissionMu.Lock()
		defer admissionMu.Unlock()
		if closed {
			return errors.New("CPR websocket is closed")
		}
		if cprInvalidJSONKeys(frame) {
			return errors.Join(ErrCPRRelay, NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "invalid or duplicate JSON keys", nil))
		}
		if strings.TrimSpace(gjson.GetBytes(frame, "type").String()) != "response.create" {
			return nil
		}
		if !lifecycle.beginResponseCreate(nil) {
			return NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "overlapping CPR turns are not supported", nil)
		}
		admitted := false
		defer func() {
			if !admitted {
				lifecycle.cancelResponseCreate()
			}
		}()
		n := int(accepted.Load()) + 1
		requested := meta.requestModelForFrame(frame)
		started := time.Now()
		if hooks.BeforePayloadParse != nil {
			if err := hooks.BeforePayloadParse(n, frame, requested); err != nil {
				return err
			}
		}
		if hooks.BeforeRequest != nil {
			if err := hooks.BeforeRequest(n, frame, requested); err != nil {
				return err
			}
		}
		if hooks.MapRequestModel != nil {
			if _, err := hooks.MapRequestModel(n, requested); err != nil {
				return err
			}
		}
		if hooks.BeforeTurn != nil {
			if err := hooks.BeforeTurn(n); err != nil {
				return err
			}
		}
		meta.captureRequestedReasoningEffort(frame, requested)
		meta.updateFromResponseCreate(frame, requested, requested)
		billing, _ := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(frame, requested)
		imageBilling.Store(&billing)
		currentStart.Store(started.UnixNano())
		nextStart.Store(&started)
		accepted.Store(int32(n))
		admitted = true
		return nil
	}
	var turnHeaders http.Header
	images := newOpenAIImageOutputCounter()
	_, relayExit := openaiwsv2.RunEntry(openaiwsv2.EntryInput{
		Ctx: ctx, ClientConn: client, UpstreamConn: server, FirstClientMessage: first,
		Options: openaiwsv2.RelayOptions{
			WriteTimeout: s.openAIWSWriteTimeout(), UpstreamDrainTimeout: time.Millisecond,
			FirstTurnStartedAt: start, BareErrorEndsTurn: true,
			TakeNextTurnStartedAt: func() time.Time {
				if t := nextStart.Swap(nil); t != nil {
					return *t
				}
				return time.Time{}
			},
			BeforeWriteClient: func(kind coderws.MessageType, frame []byte, _ bool) error {
				if kind == coderws.MessageText {
					images.AddSSEData(frame)
				}
				if kind == coderws.MessageText && gjson.GetBytes(frame, "type").String() == "response.metadata" {
					turnHeaders = make(http.Header)
					gjson.GetBytes(frame, "headers").ForEach(func(key, value gjson.Result) bool {
						if value.IsArray() {
							for _, v := range value.Array() {
								turnHeaders.Add(key.String(), v.String())
							}
						} else {
							turnHeaders.Set(key.String(), value.String())
						}
						return true
					})
				}
				return nil
			},
			OnTurnComplete: func(turn openaiwsv2.RelayTurnResult) {
				n := accepted.Load()
				if settled.Load() >= n {
					return
				}
				settled.Store(n)
				model, _ := meta.turnModels(turn.RequestModel)
				result := &OpenAIForwardResult{RequestID: turn.RequestID, Model: model,
					ResponseID: turn.RequestID, UpstreamResponseModel: turn.ResponseModel,
					UpstreamResponseModelConflict: turn.ResponseModelConflict,
					UpstreamResponseServiceTier:   turn.ResponseServiceTier,
					UpstreamTerminalEvent:         turn.TerminalEventType, UpstreamHeaders: turnHeaders,
					ServiceTier: meta.serviceTier.Load(), ReasoningEffort: meta.reasoningEffort.Load(),
					RequestedReasoningEffort: meta.requestedReasoningEffort.Load(),
					Stream:                   true, OpenAIWSMode: true, Duration: turn.Duration, FirstTokenMs: turn.FirstTokenMs,
					Usage: OpenAIUsage{InputTokens: turn.Usage.InputTokens, OutputTokens: turn.Usage.OutputTokens,
						CacheReadInputTokens: turn.Usage.CacheReadInputTokens, CacheCreationInputTokens: turn.Usage.CacheCreationInputTokens,
						ImageInputTokens: turn.Usage.ImageInputTokens, ImageOutputTokens: turn.Usage.ImageOutputTokens},
				}
				if handshake != nil {
					result.ResponseHeaders = handshake.Header.Clone()
				}
				if images.Count() > 0 {
					result.ImageCount, result.ImageOutputSizes = images.Count(), images.Sizes()
					if billing := imageBilling.Load(); billing != nil {
						result.ImageSize, result.ImageInputSize, result.BillingModel = billing.SizeTier, billing.InputSize, billing.Model
					}
				}
				images = newOpenAIImageOutputCounter()
				if strings.TrimSpace(result.RequestID) == "" {
					result.RequestID = "cprws:" + uuid.NewString()
				}
				turnHeaders = nil
				if hooks.TurnStarted != nil {
					hooks.TurnStarted(int(n), time.Unix(0, currentStart.Load()))
				}
				if hooks.AfterTurn != nil {
					hooks.AfterTurn(int(n), result, nil)
				}
			},
			BeforeClientWrite: func(kind coderws.MessageType, frame []byte) {
				if kind == coderws.MessageText && cprWSTerminal(frame) {
					lifecycle.beginTerminalWrite()
				}
			},
			AfterClientWrite: func(kind coderws.MessageType, frame []byte, err error) {
				if kind == coderws.MessageText && cprWSTerminal(frame) {
					lifecycle.finishTerminalWrite(err == nil, nil)
				}
			},
		},
	})
	admissionMu.Lock()
	closed = true
	admissionMu.Unlock()
	if n := accepted.Load(); settled.Load() < n && hooks.AfterTurn != nil {
		hooks.AfterTurn(int(n), nil, errors.New("CPR turn ended before final usage"))
	}
	if relayExit != nil && relayExit.Err != nil {
		return relayExit.Err
	}
	return nil
}

func cprWSTerminal(frame []byte) bool {
	switch gjson.GetBytes(frame, "type").String() {
	case "response.completed", "response.failed", "response.incomplete", "response.done", "error":
		return true
	}
	return false
}
