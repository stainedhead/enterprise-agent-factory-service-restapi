package condmsg_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/condmsg"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name, in string
		want     string // exact, when set
		absent   []string
	}{
		{
			name: "aws access denied",
			in: "operation error IAM: CreateRole, https response error StatusCode: 403, api error AccessDenied: " +
				"User: arn:aws:sts::111122223333:assumed-role/afs-reconciler/session is not authorized to perform: " +
				"iam:CreateRole on resource: arn:aws:iam::111122223333:role/afs-t-a",
			absent: []string{"111122223333", "arn:aws", "afs-reconciler/session"},
		},
		{name: "bare account id", in: "account 111122223333 denied", want: "account <account-id> denied"},
		{name: "govcloud arn", in: "x arn:aws-us-gov:s3:::bucket/key y", want: "x <arn> y"},
		{name: "short number kept", in: "status 403 after 12345 ms", want: "status 403 after 12345 ms"},
		{name: "13 digits kept", in: "id 1111222233334", want: "id 1111222233334"},
		{name: "plain", in: "namespace not found", want: "namespace not found"},
		{name: "empty", in: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := condmsg.Sanitize(tc.in)
			if tc.want != "" || tc.in == "" {
				if got != tc.want {
					t.Errorf("Sanitize = %q, want %q", got, tc.want)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("Sanitize = %q still contains %q", got, a)
				}
			}
		})
	}
}

func TestSanitizeNoTwelveDigitRunRemains(t *testing.T) {
	got := condmsg.Sanitize("a 123456789012 b arn:aws:iam::123456789012:role/x c 999988887777")
	for i := 0; i+12 <= len(got); i++ {
		if strings.Trim(got[i:i+12], "0123456789") == "" {
			t.Fatalf("12-digit run in %q", got)
		}
	}
}

func TestSanitizeCutsToLimitOnRuneBoundary(t *testing.T) {
	got := condmsg.Sanitize(strings.Repeat("é", 1000))
	if len(got) > condmsg.MaxBytes || len(got) < condmsg.MaxBytes-3 || !utf8.ValidString(got) {
		t.Errorf("len=%d valid=%v", len(got), utf8.ValidString(got))
	}
	if got := condmsg.Sanitize(strings.Repeat("a", 512)); len(got) != 512 {
		t.Errorf("512 bytes cut to %d", len(got))
	}
}

func TestInjectedScrubberRunsFirst(t *testing.T) {
	condmsg.SetScrubber(func(s string) string { return strings.ReplaceAll(s, "SECRET", "<redacted>") })
	t.Cleanup(func() { condmsg.SetScrubber(nil) })
	if got := condmsg.Sanitize("token SECRET for 111122223333"); got != "token <redacted> for <account-id>" {
		t.Errorf("got %q", got)
	}
}
