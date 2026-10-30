package service

import (
	"errors"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// A client key can address an entire CPR pool. No remote account quota is inferred.
type CPRKeyUsage struct {
	Mode           string `json:"mode"`
	QuotaSource    string `json:"quota_source"`
	QuotaAvailable bool   `json:"quota_available"`
}

func cprKeyUsage() *UsageInfo {
	return &UsageInfo{CPR: &CPRKeyUsage{Mode: "client_key", QuotaSource: "cpr", QuotaAvailable: false}}
}

var cprRetiredCredentialKeys = []string{"base_url", "admin_base_url", "admin_api_key", "cpr_account_id"}

func ValidateCPRCredentials(platform, kind string, credentials map[string]any) error {
	if err := validateCPRAccountShape(platform, kind); err != nil {
		return err
	}
	if kind != AccountTypeCPR {
		return nil
	}
	key, ok := credentials[cprCredentialClientKey].(string)
	if !ok || strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n\t") {
		return infraerrors.BadRequest("CPR_CLIENT_KEY_REQUIRED", "a valid CPR client API key is required")
	}
	for _, name := range cprRetiredCredentialKeys {
		if value, exists := credentials[name]; exists && value != nil && value != "" {
			return infraerrors.BadRequest("CPR_LEGACY_BINDING_UNSUPPORTED",
				"CPR uses the global gateway and an existing client API key; remove per-account URLs, Admin Key and remote account ID")
		}
	}
	return nil
}

// Remove retired secrets on an explicit edit; never rewrite remote resources.
func cleanCPRCredentials(credentials map[string]any) map[string]any {
	clean := make(map[string]any, len(credentials))
	for key, value := range credentials {
		clean[key] = value
	}
	for _, key := range cprRetiredCredentialKeys {
		delete(clean, key)
	}
	return clean
}

func validateCPRUpdate(existing *Account, input *UpdateAccountInput) error {
	kind := input.Type
	if kind == "" {
		kind = existing.Type
	}
	if kind != existing.Type && (kind == AccountTypeCPR || existing.IsCPR()) {
		return errors.New("create a new account to change a CPR channel type")
	}
	return nil
}
