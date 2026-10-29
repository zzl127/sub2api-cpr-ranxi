//go:build unit

package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func cprTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Security.URLAllowlist.AllowPrivateHosts = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	return cfg
}

func fusionCPRAccount() *Account {
	return &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeCPR,
		Credentials: map[string]any{
			"base_url": "http://127.0.0.1:18090", "cpr_account_id": "acct_fixture",
			"api_key": "fixture-client-not-a-secret", "admin_api_key": "fixture-admin-not-a-secret",
		}}
}

func TestCPRFusionBillingKeepsCodexContract(t *testing.T) {
	for _, tc := range []struct{ requested, observed, billed string }{
		{"priority", "default", "priority"},
		{"priority", "flex", "flex"},
		{"", "priority", ""},
		{"default", "default", "default"},
	} {
		r := ResolveOpenAIServiceTierBilling(fusionCPRAccount(), tc.requested, tc.observed)
		require.Equal(t, tc.billed, r.Billing)
	}
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.Equal(t, "default", ResolveOpenAIServiceTierBilling(a, "priority", "default").Billing)
}

func TestCPRFusionDoesNotBecomeOAuth(t *testing.T) {
	a := fusionCPRAccount()
	require.True(t, a.IsCPR())
	require.False(t, a.IsOpenAIOAuthLike())
	require.False(t, a.UsesOpenAICodexProtocol())
	require.True(t, IsSensitiveCredentialKey("admin_api_key"))
	require.NoError(t, ValidateCPRCredentials(a.Platform, a.Type, a.Credentials))
	require.Error(t, ValidateCPRCredentials(PlatformAnthropic, a.Type, a.Credentials))
	delete(a.Credentials, "api_key")
	require.Error(t, ValidateCPRCredentials(a.Platform, a.Type, a.Credentials))
}

func TestCPRFusionTransportCloneRejectsInvalidDefaults(t *testing.T) {
	_, err := cloneCPRHTTPTransport(nil)
	require.ErrorIs(t, err, ErrCPRNotConfigured)
	var typedNil *http.Transport
	_, err = cloneCPRHTTPTransport(typedNil)
	require.ErrorIs(t, err, ErrCPRNotConfigured)
	base := &http.Transport{}
	transport, err := cloneCPRHTTPTransport(base)
	require.NoError(t, err)
	transport.DisableCompression = true
	require.False(t, base.DisableCompression)
}

func TestCPRFusionInvalidSnapshotLockReturnsError(t *testing.T) {
	s := &AccountUsageService{}
	a := fusionCPRAccount()
	s.cprSnapshotLocks.Store(a.ID, "invalid")
	_, err := s.getCPRUsage(context.Background(), a, false)
	require.ErrorContains(t, err, "snapshot lock unavailable")
}

func TestCPRFusionAdminRootDoesNotInheritInferenceVersion(t *testing.T) {
	a := fusionCPRAccount()
	a.Credentials["base_url"] = "https://cpr.example.invalid/proxy/v1/"
	require.Equal(t, "https://cpr.example.invalid/proxy", a.GetCPRAdminBaseURL())
	a.Credentials["admin_base_url"] = "https://admin.example.invalid/separate"
	require.Equal(t, "https://admin.example.invalid/separate", a.GetCPRAdminBaseURL())
}

func TestCPRFusionMappingCannotBeRepointed(t *testing.T) {
	a := fusionCPRAccount()
	require.NoError(t, validateCPRUpdate(a, &UpdateAccountInput{Name: "new name"}))
	require.Error(t, validateCPRUpdate(a, &UpdateAccountInput{Type: AccountTypeAPIKey}))
	require.Error(t, validateCPRUpdate(a, &UpdateAccountInput{Credentials: map[string]any{
		"base_url": "https://other.invalid", "cpr_account_id": "acct_fixture",
	}}))
	creds := MergePreservingSensitiveCreds(a.Credentials, map[string]any{
		"base_url": a.GetCPRGatewayBaseURL(), "cpr_account_id": a.GetCPRAccountID(),
	})
	require.NoError(t, validateCPRUpdate(a, &UpdateAccountInput{Credentials: creds}))
}

func TestCPRFusionHTTPPreservesCompressedWireAndCallerIdentity(t *testing.T) {
	plain := []byte(` { "model":"gpt-6-astra","service_tier":"priority","stream":false,"input":"fixture" } `)
	var compressed bytes.Buffer
	encoder := gzip.NewWriter(&compressed)
	_, err := encoder.Write(plain)
	require.NoError(t, err)
	require.NoError(t, encoder.Close())
	wire := append([]byte(nil), compressed.Bytes()...)
	responseBody := []byte(`{"id":"resp_fixture","model":"gpt-6-astra","service_tier":"default","usage":{"input_tokens":13,"output_tokens":7}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		require.Equal(t, wire, body)
		require.Equal(t, "gzip", r.Header.Get("Content-Encoding"))
		require.Equal(t, "original-client/1", r.UserAgent())
		require.Equal(t, "original-turn-state", r.Header.Get("x-codex-turn-state"))
		require.Equal(t, "Bearer fixture-client-not-a-secret", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("x-api-key"))
		require.Equal(t, "a=1&a=2", r.URL.RawQuery)
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()
	a := fusionCPRAccount()
	a.Credentials["base_url"] = upstream.URL
	req := httptest.NewRequest("POST", "/v1/responses?a=1&a=2", bytes.NewReader(wire))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("User-Agent", "original-client/1")
	req.Header.Set("x-codex-turn-state", "original-turn-state")
	req.Header.Set("x-api-key", "downstream-key")
	req = httputil.CaptureWireRequest(req)
	decoded, err := httputil.ReadRequestBodyWithPrealloc(req)
	require.NoError(t, err)
	require.Equal(t, plain, decoded)
	req.Header.Set("User-Agent", "rewritten-by-another-handler")
	req.Body = httputil.NewPrereadBody([]byte(`{"model":"rewritten"}`))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req
	s := &OpenAIGatewayService{cfg: cprTestConfig()}
	result, err := s.forwardCPR(context.Background(), c, a, []byte(`{"model":"rewritten"}`))
	require.NoError(t, err)
	require.Equal(t, responseBody, recorder.Body.Bytes())
	require.Equal(t, "gpt-6-astra", result.Model)
	require.Equal(t, "priority", *result.ServiceTier)
	require.Equal(t, "default", result.UpstreamResponseServiceTier)
	require.Equal(t, 13, result.Usage.InputTokens)
	require.Nil(t, result.FirstTokenMs)
}

func TestCPRFusionHTTPDoesNotRetryOrWrapUpstreamError(t *testing.T) {
	var requests atomic.Int32
	const body = "original upstream capacity error\r\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()
	a := fusionCPRAccount()
	a.Credentials["base_url"] = upstream.URL
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	result, err := (&OpenAIGatewayService{cfg: cprTestConfig()}).forwardCPR(context.Background(), c, a, []byte(`{"model":"gpt-6-astra"}`))
	require.ErrorIs(t, err, ErrCPRRelay)
	require.NotNil(t, result)
	require.Equal(t, 429, recorder.Code)
	require.Equal(t, body, recorder.Body.String())
	require.Equal(t, "7", recorder.Header().Get("Retry-After"))
	require.EqualValues(t, 1, requests.Load())
}

func TestCPRFusionDoesNotForwardAfterLocalResponseStarted(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	a := fusionCPRAccount()
	a.Credentials["base_url"] = upstream.URL
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	_, _ = c.Writer.Write([]byte(": prior response\n\n"))
	_, err := (&OpenAIGatewayService{cfg: cprTestConfig()}).forwardCPR(context.Background(), c, a, []byte(`{"model":"gpt-6-astra"}`))
	require.ErrorIs(t, err, ErrCPRRelay)
	require.Zero(t, calls.Load())
}

func TestCPRFusionUsesRanxiSemanticTTFT(t *testing.T) {
	s := &OpenAIGatewayService{cfg: cprTestConfig()}
	options, cleanup, err := s.cprRelayOptions(context.Background(), fusionCPRAccount())
	defer cleanup()
	require.NoError(t, err)
	for _, payload := range []string{
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"type":"response.completed","response":{"output":[]}}`,
		`{"type":"response.failed","response":{"error":{"message":"failed"}}}`,
	} {
		event := ""
		switch {
		case bytes.Contains([]byte(payload), []byte("response.created")):
			event = "response.created"
		case bytes.Contains([]byte(payload), []byte("response.output_item.added")):
			event = "response.output_item.added"
		case bytes.Contains([]byte(payload), []byte("response.output_text.delta")):
			event = "response.output_text.delta"
		case bytes.Contains([]byte(payload), []byte("response.failed")):
			event = "response.failed"
		default:
			event = "response.completed"
		}
		require.Equal(t, openAIStreamDataStartsTTFT(payload, event, event == "response.failed", OpenAITTFTModeSemantic), options.StartsFirstToken([]byte(payload)))
	}
	require.False(t, options.StartsFirstToken([]byte(`{"type":"codex.response.metadata"}`)))
	require.Greater(t, options.Client.Transport.(*http.Transport).ResponseHeaderTimeout, time.Duration(0))
}

func TestCPRFusionRejectsAmbiguousJSONWithoutRewriting(t *testing.T) {
	require.True(t, cprInvalidJSONKeys([]byte(`{"model":"allowed","model":"blocked"}`)))
	require.True(t, cprInvalidJSONKeys([]byte(`{"model":"allowed","tools":[{"name":"a","name":"b"}]}`)))
	require.False(t, cprInvalidJSONKeys([]byte(`{"model":"allowed","tools":[{"name":"a"},{"name":"b"}],"empty":{}}`)))
	require.True(t, cprInvalidJSONKeys([]byte(`{"model":`)))
}

func TestCPRFusionLegacyBillingGoldenValues(t *testing.T) {
	billing := NewBillingService(&config.Config{}, &PricingService{})
	tokens := UsageTokens{InputTokens: 9816, CacheReadTokens: 302592, OutputTokens: 1325}
	for _, tc := range []struct {
		requested string
		want      float64
	}{
		{"priority", 1.801758}, {"default", 0.900879},
	} {
		tier := ResolveOpenAIServiceTierBilling(fusionCPRAccount(), tc.requested, "default").Billing
		cost, err := billing.CalculateCostWithServiceTier("gpt-6-astra", tokens, 1, tier)
		require.NoError(t, err)
		require.InDelta(t, tc.want, cost.ActualCost, 1e-8)
	}
	cost, err := billing.CalculateCostWithServiceTier("gpt-6-astra", UsageTokens{InputTokens: 13, OutputTokens: 7}, 1, "priority")
	require.NoError(t, err)
	require.InDelta(t, 0.00096, cost.ActualCost, 1e-8)
}

func TestCPRFusionObservesImagesWithoutChangingResponse(t *testing.T) {
	const body = `{"data":[{"url":"https://image.example.invalid/1","size":"1024x1024"},{"url":"https://image.example.invalid/2","size":"1024x1024"}],"usage":{"input_tokens":30,"output_tokens":50,"input_tokens_details":{"image_tokens":20,"cached_tokens":10,"cached_tokens_details":{"image_tokens":8}},"output_tokens_details":{"image_tokens":50}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()
	a := fusionCPRAccount()
	a.Credentials["base_url"] = upstream.URL
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
	result, err := (&OpenAIGatewayService{cfg: cprTestConfig()}).forwardCPR(context.Background(), c, a,
		[]byte(`{"model":"gpt-image-2","size":"1024x1024","n":2}`))
	require.NoError(t, err)
	require.Equal(t, body, recorder.Body.String())
	require.Equal(t, 2, result.ImageCount)
	require.Equal(t, []string{"1024x1024", "1024x1024"}, result.ImageOutputSizes)
	require.Equal(t, "gpt-image-2", result.BillingModel)
	require.Equal(t, 8, result.Usage.ImageCacheReadTokens)
	require.Equal(t, 20, result.Usage.ImageInputTokens)
	require.Equal(t, 50, result.Usage.ImageOutputTokens)
	require.False(t, result.UsageUnavailable)
}

func TestCPRFusionMissingObservationIsNotKnownZeroUsage(t *testing.T) {
	for _, body := range []string{
		`{"id":"empty-usage","output":[]}`,
		`{"padding":"` + strings.Repeat("x", 4<<20) + `","usage":{"input_tokens":5,"output_tokens":2}}`,
	} {
		t.Run(fmt.Sprintf("bytes_%d", len(body)), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			a := fusionCPRAccount()
			a.Credentials["base_url"] = upstream.URL
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			result, err := (&OpenAIGatewayService{cfg: cprTestConfig()}).forwardCPR(context.Background(), c, a,
				[]byte(`{"model":"gpt-6-astra"}`))
			require.NoError(t, err)
			require.Equal(t, body, recorder.Body.String())
			require.True(t, result.UsageUnavailable)
		})
	}
}
