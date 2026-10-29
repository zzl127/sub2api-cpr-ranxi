package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/rawproxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

var ErrCPRRelay = errors.New("CPR relay ended without local failover")

const CPRWireGroupKey = "cpr_wire_group"

func (s *OpenAIGatewayService) cprRelayOptions(ctx context.Context, a *Account) (rawproxy.Options, func(), error) {
	base, err := s.validatedCPRBaseURL(a)
	if err != nil {
		return rawproxy.Options{}, func() {}, err
	}
	token := a.GetCPRClientKey()
	if token == "" {
		return rawproxy.Options{}, func() {}, ErrCPRNotConfigured
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression, transport.Proxy = true, nil
	if a.Proxy != nil {
		proxy, err := url.Parse(a.Proxy.URL())
		if err != nil {
			return rawproxy.Options{}, transport.CloseIdleConnections, ErrCPRNotConfigured
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	transport.ResponseHeaderTimeout = 600 * time.Second
	mode := s.openAITTFTMode(ctx)
	return rawproxy.Options{BaseURL: base, Token: token, Client: &http.Client{Transport: transport},
		StartsFirstToken: func(payload []byte) bool {
			event := gjson.GetBytes(payload, "type").String()
			if strings.HasPrefix(event, "codex.") || event == "keepalive" {
				return false
			}
			if strings.HasPrefix(event, "response.") {
				return openAIStreamDataStartsTTFT(string(payload), event, event == "response.failed", mode)
			}
			return rawproxy.DefaultStartsFirstToken(payload)
		}}, transport.CloseIdleConnections, nil
}

func cprForwardResult(sample rawproxy.Sample, model, path string, started time.Time, duration time.Duration, stream, ws bool) *OpenAIForwardResult {
	if model == "" {
		model = sample.Model
	}
	result := &OpenAIForwardResult{RequestID: "cprraw:" + uuid.NewString(),
		ResponseID: sample.ResponseID, UpstreamHeaders: sample.UpstreamHeaders,
		Model: model, UpstreamResponseModel: sample.Model,
		UpstreamResponseServiceTier: sample.ServiceTier, UpstreamEndpoint: path,
		Stream: stream, OpenAIWSMode: ws, UpstreamTerminalEvent: sample.Event,
		Duration: duration, Usage: OpenAIUsage{
			InputTokens: sample.Usage.Input, OutputTokens: sample.Usage.Output,
			CacheReadInputTokens: sample.Usage.CacheRead, CacheCreationInputTokens: sample.Usage.CacheCreation,
			ImageInputTokens: sample.Usage.ImageInput, ImageOutputTokens: sample.Usage.ImageOutput,
			ImageCacheReadTokens: sample.Usage.ImageCacheRead,
		}}
	if sample.FirstToken != nil && stream {
		ms := int(sample.FirstToken.Milliseconds())
		result.FirstTokenMs = &ms
	}
	if ws && sample.ResponseID != "" {
		result.RequestID = "cprws:" + sample.ResponseID
	}
	return result
}

func (s *OpenAIGatewayService) forwardCPR(ctx context.Context, c *gin.Context, a *Account, fallback []byte) (*OpenAIForwardResult, error) {
	if c.Writer.Written() {
		return nil, errors.Join(ErrCPRRelay, errors.New("CPR cannot replace a response already started"))
	}
	options, closeTransport, err := s.cprRelayOptions(ctx, a)
	defer closeTransport()
	if err != nil {
		return nil, err
	}
	wire, plain := fallback, fallback
	req := c.Request.Clone(ctx)
	if original := httputil.OriginalWireRequest(c.Request); original != nil {
		options.Headers = original.Header
		u := original.URL
		req.URL = &u
		if original.Read {
			wire, plain = original.Body, original.Plain
		}
	}
	if key := getAPIKeyFromContext(c); key != nil {
		options.DownstreamToken = key.Key
	}
	if strings.HasPrefix(strings.TrimSpace(string(plain)), "{") && cprInvalidJSONKeys(plain) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "Invalid or duplicate JSON object keys"}})
		MarkResponseCommitted(c)
		return nil, ErrCPRRelay
	}
	started := time.Now()
	images := newOpenAIImageOutputCounter()
	options.ObservePayload = func(payload []byte) {
		images.AddJSONResponse(payload)
		images.AddSSEData(payload)
	}
	r, err := rawproxy.HTTP(ctx, c.Writer, req, wire, options)
	if c.Writer.Written() {
		MarkResponseCommitted(c)
	}
	if r == nil {
		if !c.Writer.Written() {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "message": "CPR connection unavailable"}})
			MarkResponseCommitted(c)
		}
		return nil, ErrCPRRelay
	}
	result := cprForwardResult(r.Sample, gjson.GetBytes(plain, "model").String(),
		req.URL.Path, started, r.Duration, gjson.GetBytes(plain, "stream").Bool(), false)
	if images.Count() > 0 {
		billing, _ := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(plain, result.Model)
		result.ImageCount, result.ImageOutputSizes = images.Count(), images.Sizes()
		result.ImageSize, result.ImageInputSize, result.BillingModel = billing.SizeTier, billing.InputSize, billing.Model
	}
	result.ClientDisconnect = r.ClientDisconnect
	if r.TerminalEvent != "" {
		result.UpstreamTerminalEvent = r.TerminalEvent
	}
	failedTerminal := r.TerminalEvent == "response.failed" || r.TerminalEvent == "response.incomplete" || r.TerminalEvent == "error"
	result.streamReadIncomplete = (result.Stream && !r.Terminal) || failedTerminal
	result.ResponseHeaders = r.Headers
	if tier := gjson.GetBytes(plain, "service_tier").String(); tier != "" {
		result.ServiceTier = &tier
	}
	effort := gjson.GetBytes(plain, "reasoning.effort").String()
	if effort == "" {
		effort = gjson.GetBytes(plain, "reasoning_effort").String()
	}
	if effort != "" {
		result.ReasoningEffort, result.RequestedReasoningEffort = &effort, &effort
	}
	SetActualOpenAIUpstreamEndpoint(c, req.URL.Path)
	if ((!r.Found && result.ImageCount == 0) || r.ObservationLimited) && req.Method == http.MethodPost && r.StatusCode >= 200 && r.StatusCode < 300 &&
		!strings.HasSuffix(req.URL.Path, "/count_tokens") && !strings.HasSuffix(req.URL.Path, "/input_tokens") &&
		!strings.HasSuffix(req.URL.Path, "/alpha/search") {
		result.UsageUnavailable = true
		logger.L().Warn("cpr_usage_observation_missing", zap.Int64("account_id", a.ID),
			zap.String("endpoint", req.URL.Path), zap.String("request_id", result.RequestID),
			zap.Bool("observation_limited", r.ObservationLimited))
	}
	if result.streamReadIncomplete || r.StatusCode >= http.StatusBadRequest || (err != nil && !r.Terminal) {
		return result, ErrCPRRelay
	}
	return result, nil
}

func (s *OpenAIGatewayService) CPRGroupHasAccount(ctx context.Context, groupID *int64) (bool, error) {
	if groupID == nil || s.accountRepo == nil {
		return false, nil
	}
	if inspector, ok := s.accountRepo.(interface {
		HasCPRInGroup(context.Context, int64) (bool, error)
	}); ok {
		return inspector.HasCPRInGroup(ctx, *groupID)
	}
	accounts, err := s.accountRepo.ListByGroup(ctx, *groupID)
	if err != nil {
		return false, err
	}
	for i := range accounts {
		if accounts[i].IsCPR() {
			return true, nil
		}
	}
	return false, nil
}

// Raw forwarding must not let the admission parser read the first model key
// while the upstream executes a later duplicate. No payload is normalized.
func cprInvalidJSONKeys(body []byte) bool {
	if !json.Valid(body) {
		return true
	}
	type frame struct {
		keys    map[string]struct{}
		wantKey bool
	}
	var stack []*frame
	decoder := json.NewDecoder(bytes.NewReader(body))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return len(stack) != 0
		}
		if err != nil {
			return true
		}
		switch t := token.(type) {
		case json.Delim:
			switch t {
			case '{':
				if len(stack) >= 256 {
					return true
				}
				stack = append(stack, &frame{keys: map[string]struct{}{}, wantKey: true})
				continue
			case '[':
				if len(stack) >= 256 {
					return true
				}
				stack = append(stack, nil)
				continue
			default:
				if len(stack) == 0 {
					return true
				}
				stack = stack[:len(stack)-1]
			}
		case string:
			if n := len(stack); n > 0 && stack[n-1] != nil && stack[n-1].wantKey {
				if _, exists := stack[n-1].keys[t]; exists {
					return true
				}
				stack[n-1].keys[t], stack[n-1].wantKey = struct{}{}, false
				continue
			}
		}
		if n := len(stack); n > 0 && stack[n-1] != nil {
			stack[n-1].wantKey = true
		}
	}
}

// Model discovery has no request model to use for account selection. Only a
// dedicated CPR group can expose an upstream manifest without mixing providers.
func (s *OpenAIGatewayService) CPRModelRelayGroup(ctx context.Context, groupID *int64) (bool, error) {
	if groupID == nil || s.accountRepo == nil {
		return false, nil
	}
	accounts, err := s.accountRepo.ListByGroup(ctx, *groupID)
	if err != nil {
		return false, err
	}
	found := false
	for i := range accounts {
		if !accounts[i].IsCPR() {
			return false, nil
		}
		found = true
	}
	return found, nil
}

func (s *OpenAIGatewayService) ForwardCPRModels(ctx context.Context, c *gin.Context, a *Account) error {
	if !a.IsCPR() {
		return ErrCPRNotConfigured
	}
	latest, err := s.latestOpenAITurnAccount(ctx, c, a)
	if err != nil {
		return err
	}
	if !latest.IsCPR() {
		return ErrCPRNotConfigured
	}
	_, err = s.forwardCPR(ctx, c, latest, nil)
	return err
}
