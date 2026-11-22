//go:build conformance

package conformance

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// anthropicNoRetry stops the SDK retrying a refusal, so one call is one request.
func anthropicNoRetry() option.RequestOption { return option.WithMaxRetries(0) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
