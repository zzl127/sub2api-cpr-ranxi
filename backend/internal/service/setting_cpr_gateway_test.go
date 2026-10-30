//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type cprSettingsRepo struct {
	SettingRepository
	mu       sync.Mutex
	values   map[string]string
	readErr  error
	writeErr error
}

func (r *cprSettingsRepo) GetValue(ctx context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if r.readErr != nil {
		return "", r.readErr
	}
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *cprSettingsRepo) Set(ctx context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.writeErr != nil {
		return r.writeErr
	}
	r.values[key] = value
	return nil
}

func (r *cprSettingsRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.readErr != nil {
		return nil, r.readErr
	}
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			result[key] = value
		}
	}
	return result, nil
}

func fusionCPRService(base string) *OpenAIGatewayService {
	cfg := cprTestConfig()
	repo := &cprSettingsRepo{values: map[string]string{SettingKeyCPRGatewayBaseURL: base}}
	return &OpenAIGatewayService{cfg: cfg, settingService: NewSettingService(repo, cfg)}
}

func TestCPRGatewaySettingsPersistAndHotReload(t *testing.T) {
	s := fusionCPRService("")
	initial, err := s.GetCPRGateway(t.Context())
	require.NoError(t, err)
	require.Empty(t, initial.BaseURL)
	saved, err := s.SetCPRGateway(t.Context(), CPRGatewaySettings{BaseURL: " http://127.0.0.1:18090/v1/ "})
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:18090/v1", saved.BaseURL)
	peer := &OpenAIGatewayService{cfg: s.cfg, settingService: NewSettingService(s.settingService.settingRepo, s.cfg)}
	for _, gateway := range []*OpenAIGatewayService{s, peer} {
		actual, err := gateway.validatedCPRBaseURL(t.Context(), fusionCPRAccount())
		require.NoError(t, err)
		require.Equal(t, saved.BaseURL, actual)
	}
	_, err = peer.SetCPRGateway(t.Context(), CPRGatewaySettings{BaseURL: "https://new.example.invalid"})
	require.NoError(t, err)
	actual, err := s.validatedCPRBaseURL(t.Context(), fusionCPRAccount())
	require.NoError(t, err)
	require.Equal(t, "https://new.example.invalid", actual)
	_, err = s.SetCPRGateway(t.Context(), CPRGatewaySettings{})
	require.NoError(t, err)
	_, err = peer.validatedCPRBaseURL(t.Context(), fusionCPRAccount())
	require.ErrorIs(t, err, ErrCPRNotConfigured)
}

func TestCPRGatewaySettingsFailClosedOnStorageErrors(t *testing.T) {
	s := fusionCPRService("https://cpr.example.invalid")
	repo := s.settingService.settingRepo.(*cprSettingsRepo)
	repo.writeErr = errors.New("sensitive internal database error")
	_, err := s.SetCPRGateway(t.Context(), CPRGatewaySettings{BaseURL: "https://other.example.invalid"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "sensitive")
	repo.writeErr = nil
	settings, err := s.GetCPRGateway(t.Context())
	require.NoError(t, err)
	require.Equal(t, "https://cpr.example.invalid", settings.BaseURL)
	repo.readErr = errors.New("database unavailable")
	_, cleanup, err := s.cprRelayOptions(t.Context(), fusionCPRAccount())
	defer cleanup()
	require.Error(t, err)
	repo.readErr = nil
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.GetCPRGateway(cancelled)
	require.Error(t, err)
}

func TestCPRGatewayURLValidationDoesNotContactUpstream(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	s := fusionCPRService("")
	_, err := s.SetCPRGateway(t.Context(), CPRGatewaySettings{BaseURL: server.URL})
	require.NoError(t, err)
	for _, url := range []string{
		"ftp://cpr.example.invalid", "https://user:password@cpr.example.invalid",
		"https://cpr.example.invalid?api_key=secret", "https://cpr.example.invalid#fragment",
		"https://cpr.example.invalid?", "https://cpr.example.invalid\r\nX-Api-Key: secret", "/relative",
	} {
		_, err := s.SetCPRGateway(t.Context(), CPRGatewaySettings{BaseURL: url})
		require.Error(t, err, url)
	}
	require.Zero(t, requests.Load(), "saving settings must not probe CPR or create admin resources")
	require.Equal(t, server.URL, s.settingService.settingRepo.(*cprSettingsRepo).values[SettingKeyCPRGatewayBaseURL])
	s.cfg.Security.URLAllowlist.AllowInsecureHTTP = false
	_, err = s.SetCPRGateway(t.Context(), CPRGatewaySettings{BaseURL: server.URL})
	require.Error(t, err)
}

func TestCPRGatewayMissingNeverUsesLegacyDestination(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	a := fusionCPRAccount()
	a.Credentials["base_url"] = server.URL
	for _, s := range []*OpenAIGatewayService{fusionCPRService(""), {cfg: cprTestConfig()}} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		_, err := s.forwardCPR(t.Context(), c, a, []byte(`{"model":"gpt-6-astra"}`))
		require.Error(t, err)
	}
	require.Zero(t, requests.Load())
}

func TestCPRClientKeyCredentialsAndRetiredFields(t *testing.T) {
	require.NoError(t, ValidateCPRCredentials(PlatformOpenAI, AccountTypeCPR, map[string]any{"api_key": "existing-pool-key"}))
	for _, value := range []any{nil, "", " ", "a\r\nb", 123, true} {
		require.Error(t, ValidateCPRCredentials(PlatformOpenAI, AccountTypeCPR, map[string]any{"api_key": value}))
	}
	for _, field := range cprRetiredCredentialKeys {
		require.Error(t, ValidateCPRCredentials(PlatformOpenAI, AccountTypeCPR, map[string]any{"api_key": "existing-key", field: "legacy"}))
	}
	legacy := map[string]any{
		"api_key": "existing-key", "admin_api_key": "old-admin", "base_url": "https://old.invalid",
		"cpr_account_id": "old-account", "model_mapping": map[string]any{"alias": "model"},
	}
	merged := MergePreservingSensitiveCreds(legacy, map[string]any{"model_mapping": legacy["model_mapping"]})
	clean := cleanCPRCredentials(merged)
	require.Equal(t, "existing-key", clean["api_key"])
	require.Equal(t, legacy["model_mapping"], clean["model_mapping"])
	require.NoError(t, ValidateCPRCredentials(PlatformOpenAI, AccountTypeCPR, clean))
	require.Equal(t, "old-admin", legacy["admin_api_key"], "cleanup must not mutate a shared scheduler account")
}

func TestCPRMultipleClientKeysKeepUpstreamPoolPermissions(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.Empty(t, r.Header.Get("x-api-key"))
		require.Empty(t, r.Header.Get("chatgpt-account-id"))
		switch r.Header.Get("Authorization") {
		case "Bearer existing-pool-one":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"model not permitted for this key"}}`))
		case "Bearer existing-pool-two":
			_, _ = w.Write([]byte(`{"model":"gpt-6-astra","usage":{"input_tokens":7,"output_tokens":2}}`))
		default:
			t.Errorf("unexpected client credential")
			w.WriteHeader(http.StatusUnauthorized)
		}
		calls.Add(1)
	}))
	defer server.Close()
	s := fusionCPRService(server.URL)
	for _, key := range []string{"existing-pool-one", "existing-pool-two"} {
		a := fusionCPRAccount()
		a.Credentials = map[string]any{"api_key": key}
		require.NoError(t, ValidateCPRCredentials(a.Platform, a.Type, a.Credentials))
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		_, err := s.forwardCPR(t.Context(), c, a, []byte(`{"model":"gpt-6-astra"}`))
		if key == "existing-pool-one" {
			require.ErrorIs(t, err, ErrCPRRelay)
			require.Equal(t, http.StatusForbidden, recorder.Code)
			require.Contains(t, recorder.Body.String(), "model not permitted")
		} else {
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, recorder.Code)
		}
	}
	require.EqualValues(t, 2, calls.Load(), "no discovery, provisioning, quota calls or retry")
}
