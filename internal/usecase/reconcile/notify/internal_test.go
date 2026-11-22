package notify

import (
	"fmt"
	"log/slog"
	"testing"
	"time"
)

func TestCredentialNeverPrints(t *testing.T) {
	c := Credential("hunter2-value")
	for name, got := range map[string]string{
		"String": c.String(), "GoString": c.GoString(), "LogValue": c.LogValue().String(),
		"Sprintf": fmt.Sprintf("%v %+v %#v", c, c, c),
	} {
		if got == "" || contains(got, "hunter2") {
			t.Fatalf("%s leaked: %q", name, got)
		}
	}
	if c.Reveal() != "hunter2-value" {
		t.Fatal("Reveal")
	}
	_ = slog.Any("k", c)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestEmailSeenSetIsBounded(t *testing.T) {
	e := &EmailDrainer{}
	base := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	for i := range emailSeenMax {
		e.mark(fmt.Sprint(i), base.Add(time.Duration(i)*time.Millisecond))
	}
	// Full and nothing old enough to expire: the oldest tenth goes.
	e.mark("new", base.Add(time.Minute))
	if n := len(e.sent); n >= emailSeenMax || !e.already("new") || e.already("0") {
		t.Fatalf("len=%d", n)
	}
	// Full again with everything expired: expired entries go.
	e2 := &EmailDrainer{}
	for i := range emailSeenMax {
		e2.mark(fmt.Sprint(i), base)
	}
	e2.mark("fresh", base.Add(emailSeenTTL+time.Hour))
	if len(e2.sent) != 1 || !e2.already("fresh") {
		t.Fatalf("len=%d", len(e2.sent))
	}
}
