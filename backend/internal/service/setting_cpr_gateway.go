package service

import (
	"context"
	"errors"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const SettingKeyCPRGatewayBaseURL = "cpr_gateway_base_url"

type CPRGatewaySettings struct {
	BaseURL string `json:"base_url"`
}

// Read once per outgoing connection. Do not cache a stale credential destination
// or fall back to account-local URLs when a setting is missing or unreadable.
func (s *SettingService) GetCPRGateway(ctx context.Context) (CPRGatewaySettings, error) {
	if s == nil || s.settingRepo == nil {
		return CPRGatewaySettings{}, ErrCPRNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	value, err := s.settingRepo.GetValue(ctx, SettingKeyCPRGatewayBaseURL)
	if errors.Is(err, ErrSettingNotFound) {
		return CPRGatewaySettings{}, nil
	}
	if err != nil {
		return CPRGatewaySettings{}, errors.New("CPR gateway settings unavailable")
	}
	return CPRGatewaySettings{BaseURL: strings.TrimSpace(value)}, nil
}

func (s *SettingService) SetCPRGateway(ctx context.Context, input CPRGatewaySettings) (CPRGatewaySettings, error) {
	if s == nil || s.settingRepo == nil {
		return CPRGatewaySettings{}, ErrCPRNotConfigured
	}
	base := strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	if base != "" {
		if _, err := validateCPROutboundURL(s.cfg, base); err != nil {
			return CPRGatewaySettings{}, infraerrors.BadRequest("CPR_INVALID_GATEWAY",
				"invalid CPR gateway URL or destination denied by the server URL policy")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.settingRepo.Set(ctx, SettingKeyCPRGatewayBaseURL, base); err != nil {
		return CPRGatewaySettings{}, errors.New("CPR gateway settings could not be saved")
	}
	return CPRGatewaySettings{BaseURL: base}, nil
}

func (s *OpenAIGatewayService) GetCPRGateway(ctx context.Context) (CPRGatewaySettings, error) {
	if s == nil {
		return CPRGatewaySettings{}, ErrCPRNotConfigured
	}
	return s.settingService.GetCPRGateway(ctx)
}

func (s *OpenAIGatewayService) SetCPRGateway(ctx context.Context, input CPRGatewaySettings) (CPRGatewaySettings, error) {
	if s == nil {
		return CPRGatewaySettings{}, ErrCPRNotConfigured
	}
	return s.settingService.SetCPRGateway(ctx, input)
}
