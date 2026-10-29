//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOllamaCloudUsageHTMLCoalescedTextPreservesFields(t *testing.T) {
	// Ignored end tags repeatedly append to the same text node. Keep a bounded
	// case for the x/net/html coalescing fix without a flaky timing assertion.
	body := `<html><body><h2><span>Cloud usage</span><span>Pro</span></h2><div>` +
		strings.Repeat("x</unknown>", 20000) + `</div></body></html>`
	usage, err := parseOllamaCloudUsageHTML([]byte(body))
	require.NoError(t, err)
	require.Equal(t, "Pro", usage.Plan)
}
