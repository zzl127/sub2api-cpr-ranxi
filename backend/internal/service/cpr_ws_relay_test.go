//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCPRFusionWebSocketRoundsKeepFramesAndReleaseHooks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := []string{
		` { "type":"response.create","model":"gpt-6-astra","service_tier":"priority","input":"one" } `,
		`{"type":"response.create","model":"gpt-6-astra","input":"two"}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer fixture-client-not-a-secret", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("x-api-key"))
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for i, want := range frames {
			kind, body, err := conn.Read(ctx)
			if err != nil {
				return
			}
			require.Equal(t, coderws.MessageText, kind)
			require.Equal(t, want, string(body))
			created := fmt.Sprintf(`{"type":"response.created","response":{"id":"turn-%d","model":"gpt-6-astra","service_tier":"default"}}`, i)
			_ = conn.Write(ctx, coderws.MessageText, []byte(created))
			_, control, err := conn.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf(` { "type":"response.cancel","response_id":"turn-%d" } `, i), string(control))
			time.Sleep(10 * time.Millisecond)
			delta := fmt.Sprintf(`{"type":"response.output_text.delta","response_id":"turn-%d","delta":"ok"}`, i)
			_ = conn.Write(ctx, coderws.MessageText, []byte(delta))
			done := fmt.Sprintf(`{"type":"response.completed","response":{"id":"turn-%d","model":"gpt-6-astra","service_tier":"default","usage":{"input_tokens":13,"output_tokens":7}}}`, i)
			_ = conn.Write(ctx, coderws.MessageText, []byte(done))
		}
		_, _, _ = conn.Read(ctx)
	}))
	defer upstream.Close()
	a := fusionCPRAccount()
	a.Credentials["base_url"] = upstream.URL
	s := &OpenAIGatewayService{cfg: cprTestConfig()}
	results := make(chan *OpenAIForwardResult, 3)
	var admissions atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, first, err := conn.Read(ctx)
		if err != nil {
			return
		}
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		hooks := &OpenAIWSIngressHooks{
			InitialRequestModel: "gpt-6-astra", InitialTurnStartedAt: time.Now(),
			MapRequestModel: func(_ int, model string) (string, error) { return "must-not-rewrite-" + model, nil },
			BeforeRequest:   func(_ int, body []byte, _ string) error { require.True(t, gjson.ValidBytes(body)); return nil },
			BeforeTurn:      func(_ int) error { admissions.Add(1); return nil },
			AfterTurn: func(_ int, result *OpenAIForwardResult, _ error) {
				if result != nil {
					results <- result
				}
			},
		}
		_ = s.proxyCPRWebSocket(ctx, c, conn, a, first, hooks)
	}))
	defer proxy.Close()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	for i, frame := range frames {
		require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(frame)))
		for {
			_, body, err := conn.Read(ctx)
			require.NoError(t, err)
			if gjson.GetBytes(body, "type").String() == "response.created" {
				control := fmt.Sprintf(` { "type":"response.cancel","response_id":"turn-%d" } `, i)
				require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(control)))
			}
			if gjson.GetBytes(body, "type").String() == "response.completed" {
				break
			}
		}
		select {
		case result := <-results:
			require.Equal(t, 13, result.Usage.InputTokens)
			require.Equal(t, 7, result.Usage.OutputTokens)
			require.NotNil(t, result.FirstTokenMs)
			require.Equal(t, "gpt-6-astra", result.Model)
			if i == 0 {
				require.NotNil(t, result.ServiceTier)
				require.Equal(t, "priority", *result.ServiceTier)
			} else if result.ServiceTier != nil {
				require.NotEqual(t, "priority", *result.ServiceTier)
			}
		case <-ctx.Done():
			t.Fatal("missing turn settlement")
		}
	}
	require.EqualValues(t, 1, admissions.Load(), "first-turn admission belongs to the native HTTP handler")
	_ = conn.Close(coderws.StatusNormalClosure, "complete")
}
