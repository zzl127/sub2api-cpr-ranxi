package openai_ws_v2

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCPRFusionPreservesNativeTTFTForUnidentifiedDeltas(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, cpr := range []bool{false, true} {
		state := &relayState{bareErrorEndsTurn: cpr}
		for turn := 0; turn < 2; turn++ {
			now := start.Add(time.Duration(turn) * time.Second)
			state.setPendingTurnStartedAt(now)
			clock := func() time.Time { return now }
			observeUpstreamMessage(state, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":"r%d"}}`, turn)), start, clock, nil)
			now = now.Add(20 * time.Millisecond)
			observeUpstreamMessage(state, []byte(`{"type":"response.output_text.delta","delta":"hello"}`), start, clock, nil)
			terminal := observeUpstreamMessage(state, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"r%d","usage":{"input_tokens":1,"output_tokens":1}}}`, turn)), start, clock, nil)
			if turn == 0 {
				require.NotNil(t, terminal.firstToken)
			} else {
				// v2.17.6 does not synthesize a timestamp for this shape.
				require.Nil(t, terminal.firstToken)
			}
		}
	}
}
