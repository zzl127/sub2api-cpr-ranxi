package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

const CPRAccountSnapshotExtraKey = "cpr_account_snapshot"

// A remote observation is not a local scheduling decision or an OAuth token.
type CPRAccountSnapshot struct {
	AccountID           string                 `json:"account_id"`
	Name                string                 `json:"name,omitempty"`
	Email               string                 `json:"email,omitempty"`
	PlanType            string                 `json:"plan_type,omitempty"`
	Status              string                 `json:"status,omitempty"`
	Enabled             bool                   `json:"enabled"`
	OutboundProxy       string                 `json:"outbound_proxy,omitempty"`
	QuotaUpdatedDisplay string                 `json:"quota_updated_display,omitempty"`
	CheckedAt           *time.Time             `json:"checked_at,omitempty"`
	QueryAttemptedAt    *time.Time             `json:"query_attempted_at,omitempty"`
	QueryError          string                 `json:"query_error,omitempty"`
	FiveHour            *UsageProgress         `json:"five_hour,omitempty"`
	SevenDay            *UsageProgress         `json:"seven_day,omitempty"`
	Credits             *OpenAICredits         `json:"credits,omitempty"`
	Capabilities        CPRAccountCapabilities `json:"capabilities"`
	Binding             string                 `json:"-"`
}

func cprBinding(a *Account) string {
	if a == nil {
		return ""
	}
	raw, _ := json.Marshal([]string{a.GetCPRGatewayBaseURL(), a.GetCPRAdminBaseURL(),
		a.GetCPRAccountID(), a.GetCPRAdminAPIKey(), a.GetCPRClientKey()})
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func (s *AccountUsageService) ConfigureCPR(cfgService *OpenAIGatewayService) {
	if cfgService != nil {
		s.cprQuotaService = NewCPRQuotaService(cfgService.cfg)
	}
}

func (s *AccountUsageService) DiscoverCPR(ctx context.Context, base, key string) ([]CPRConnectAccount, error) {
	if s == nil || s.cprQuotaService == nil {
		return nil, ErrCPRNotConfigured
	}
	return s.cprQuotaService.DiscoverAccounts(ctx, base, key)
}

func (s *AccountUsageService) ConfigureCPRClient(ctx context.Context, a *Account) (string, error) {
	if s == nil || s.cprQuotaService == nil {
		return "", ErrCPRNotConfigured
	}
	return s.cprQuotaService.EnsureClientKey(ctx, a)
}

func cprQueryError(err error) string {
	switch {
	case errors.Is(err, ErrCPRAdminKeyInvalid):
		return "admin_key_invalid"
	case errors.Is(err, ErrCPRAccountMissing):
		return "account_missing"
	case errors.Is(err, ErrCPRNotConfigured):
		return "not_configured"
	default:
		return "query_failed"
	}
}

func cprWindow(window *OpenAIRateLimitWindow, observed time.Time) *UsageProgress {
	if window == nil {
		return nil
	}
	p := &UsageProgress{Utilization: window.UsedPercent}
	if window.ResetAt > 0 {
		reset := time.Unix(window.ResetAt, 0)
		p.ResetsAt = &reset
		p.RemainingSeconds = max(0, int(reset.Sub(observed).Seconds()))
	}
	return p
}

func (s *AccountUsageService) getCPRUsage(ctx context.Context, a *Account, force bool) (*UsageInfo, error) {
	lock, _ := s.cprSnapshotLocks.LoadOrStore(a.ID, &sync.Mutex{})
	mu, ok := lock.(*sync.Mutex)
	if !ok || mu == nil {
		return nil, errors.New("CPR quota snapshot lock unavailable")
	}
	mu.Lock()
	defer mu.Unlock()
	if s.cprQuotaService == nil {
		return nil, ErrCPRNotConfigured
	}
	var snap CPRAccountSnapshot
	if raw, ok := a.Extra[CPRAccountSnapshotExtraKey]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &snap)
	}
	binding := cprBinding(a)
	savedBinding, _ := a.Extra["cpr_snapshot_binding"].(string)
	if snap.AccountID != a.GetCPRAccountID() || binding != savedBinding {
		snap = CPRAccountSnapshot{AccountID: a.GetCPRAccountID()}
	}
	now := time.Now()
	if force || snap.QueryAttemptedAt == nil || now.Sub(*snap.QueryAttemptedAt) >= 10*time.Minute {
		state, err := s.cprQuotaService.FetchAccountState(ctx, a)
		if err != nil {
			snap.QueryAttemptedAt, snap.QueryError = &now, cprQueryError(err)
		} else {
			snap = CPRAccountSnapshot{AccountID: state.AccountID, Name: state.Name,
				Email: state.Email, PlanType: state.PlanType, Status: state.Status,
				Enabled: state.Enabled, OutboundProxy: state.OutboundProxyEndpoint,
				QuotaUpdatedDisplay: state.QuotaUpdatedDisplay, CheckedAt: &now,
				QueryAttemptedAt: &now, Credits: state.Credits, Capabilities: state.Capabilities}
			if state.RateLimit != nil {
				for _, w := range []*OpenAIRateLimitWindow{state.RateLimit.PrimaryWindow, state.RateLimit.SecondaryWindow} {
					if w == nil {
						continue
					}
					if w.LimitWindowSeconds < int64((24*time.Hour)/time.Second) {
						snap.FiveHour = cprWindow(w, now)
					} else {
						snap.SevenDay = cprWindow(w, now)
					}
				}
			}
		}
		if s.accountRepo != nil && a.ID > 0 {
			fresh, readErr := s.accountRepo.GetByID(ctx, a.ID)
			if readErr != nil || fresh == nil || cprBinding(fresh) != binding {
				return nil, errors.New("CPR binding changed while reading quota")
			}
			if err := s.accountRepo.UpdateExtra(ctx, a.ID, map[string]any{
				CPRAccountSnapshotExtraKey: snap, "cpr_snapshot_binding": binding,
			}); err != nil {
				snap.QueryError = "snapshot_save_failed"
			}
		}
	}
	return &UsageInfo{CPR: &snap, UpdatedAt: snap.QueryAttemptedAt,
		FiveHour: snap.FiveHour, SevenDay: snap.SevenDay}, nil
}

func ValidateCPRCredentials(platform, kind string, credentials map[string]any) error {
	if err := validateCPRAccountShape(platform, kind); err != nil {
		return err
	}
	if kind != AccountTypeCPR {
		return nil
	}
	a := &Account{Platform: platform, Type: kind, Credentials: credentials}
	if a.GetCPRClientKey() == a.GetCPRAdminAPIKey() {
		return errors.New("CPR client and admin credentials must be separate")
	}
	for _, value := range []string{a.GetCPRGatewayBaseURL(), a.GetCPRAccountID(), a.GetCPRClientKey(), a.GetCPRAdminAPIKey()} {
		if value == "" || strings.ContainsAny(value, "\r\n") {
			return ErrCPRNotConfigured
		}
	}
	return nil
}

func validateCPRUpdate(existing *Account, input *UpdateAccountInput) error {
	kind := input.Type
	if kind == "" {
		kind = existing.Type
	}
	if kind != existing.Type && (kind == AccountTypeCPR || existing.IsCPR()) {
		return errors.New("create a new account to change a CPR binding type")
	}
	if !existing.IsCPR() || input.Credentials == nil {
		return nil
	}
	next := &Account{Platform: existing.Platform, Type: kind,
		Credentials: MergePreservingSensitiveCreds(existing.Credentials, input.Credentials)}
	if next.GetCPRGatewayBaseURL() != existing.GetCPRGatewayBaseURL() ||
		next.GetCPRAdminBaseURL() != existing.GetCPRAdminBaseURL() ||
		next.GetCPRAccountID() != existing.GetCPRAccountID() {
		return errors.New("create a new CPR mapping to change its gateway or remote account")
	}
	return nil
}
