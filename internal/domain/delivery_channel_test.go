package domain_test

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

const chSecret = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001" //nolint:gosec // fixture id

func TestChannelValidation(t *testing.T) {
	slack := map[string]any{"channel": "#payments-alerts"}
	tests := []struct {
		name    string
		p       domain.NewChannelParams
		service bool
		ok      bool
	}{
		{"slack ok", domain.NewChannelParams{Name: "s", Kind: domain.ChannelSlack, Config: slack, SecretID: chSecret}, false, true},
		{"slack needs secret", domain.NewChannelParams{Name: "s", Kind: domain.ChannelSlack, Config: slack}, false, false},
		{"slack bad channel", domain.NewChannelParams{Name: "s", Kind: domain.ChannelSlack, Config: map[string]any{"channel": "x y"}, SecretID: chSecret}, false, false},
		{"inline credential", domain.NewChannelParams{Name: "s", Kind: domain.ChannelSlack, Config: map[string]any{"channel": "#a1", "token": "x"}, SecretID: chSecret}, false, false},
		{"email ok", domain.NewChannelParams{Name: "e", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"ops@example.com"}}}, false, true},
		{"email secret refused", domain.NewChannelParams{Name: "e", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"ops@example.com"}}, SecretID: chSecret}, false, false},
		{"email bad address", domain.NewChannelParams{Name: "e", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"Ops <o@example.com>"}}}, false, false},
		{"webhook url with query", domain.NewChannelParams{Name: "w", Kind: domain.ChannelWebhook, Config: map[string]any{"url": "https://h.example.com/x?token=1"}}, false, false},
		{"webhook ok", domain.NewChannelParams{Name: "w", Kind: domain.ChannelWebhook, Config: map[string]any{"url": "https://h.example.com/x"}}, false, true},
		{"jira ok", domain.NewChannelParams{Name: "j", Kind: domain.ChannelJira, Config: map[string]any{"url": "https://j.example.com", "project": "OPS"}, SecretID: chSecret}, false, true},
		{"eventbridge ok", domain.NewChannelParams{Name: "b", Kind: domain.ChannelEventBridge, Config: map[string]any{"eventBusArn": "arn:aws:events:us-east-1:111122223333:event-bus/x"}}, false, true},
		{"eventbridge bad arn", domain.NewChannelParams{Name: "b", Kind: domain.ChannelEventBridge, Config: map[string]any{"eventBusArn": "nope"}}, false, false},
		{"unknown kind", domain.NewChannelParams{Name: "b", Kind: "sms"}, false, false},
		{"event types ok", domain.NewChannelParams{Name: "e", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"a@example.com"}}, EventTypes: []string{"budget.*", "agent.status_changed", "notification.test_requested"}}, false, true},
		{"event type unknown", domain.NewChannelParams{Name: "e", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"a@example.com"}}, EventTypes: []string{"nope.*"}}, false, false},
		{"event type duplicate", domain.NewChannelParams{Name: "e", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"a@example.com"}}, EventTypes: []string{"budget.*", "budget.*"}}, false, false},
		{"service email", domain.NewChannelParams{Name: "e", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"a@example.com"}}}, true, true},
		{"service slack refused", domain.NewChannelParams{Name: "s", Kind: domain.ChannelSlack, Config: slack}, true, false},
		{"service secret refused", domain.NewChannelParams{Name: "w", Kind: domain.ChannelWebhook, Config: map[string]any{"url": "https://h.example.com/x"}, SecretID: chSecret}, true, false},
		{"empty name", domain.NewChannelParams{Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"a@example.com"}}}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate(tc.service)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && domain.KindOf(err) != domain.KindValidation {
				t.Fatalf("kind = %v", domain.KindOf(err))
			}
		})
	}
}

func TestChannelApplyAndDelivery(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	c, err := domain.NewChannelParams{Name: "e", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"a@example.com"}}, Enabled: true}.New("nch_1", "team_1", now)
	if err != nil || c.Version != 1 || c.ServiceLevel() {
		t.Fatal(c, err)
	}
	off, types := false, []string{"budget.*"}
	n, err := c.Apply(domain.ChannelPatch{Enabled: &off, EventTypes: &types}, now)
	if err != nil || n.Enabled || len(n.EventTypes) != 1 || len(c.EventTypes) != 0 {
		t.Fatal(n, err)
	}
	s := chSecret
	if _, err := c.Apply(domain.ChannelPatch{SecretID: &s}, now); err == nil {
		t.Fatal("secret on email must be refused")
	}
	if c.Delivery().Healthy != nil {
		t.Fatal("pending channel has unknown health")
	}
	fail := now.Add(-time.Minute)
	c.Status.Conditions = []domain.Condition{
		{Type: domain.CondDeliveryHealthy, Status: domain.ConditionFalse},
		domain.DeliveryDetail{LastFailureAt: &fail, LastError: "boom", LastTestID: "ntest_x"}.Condition(now),
	}
	d := c.Delivery()
	if d.Healthy == nil || *d.Healthy || d.LastError != "boom" || d.LastTestID != "ntest_x" || !d.LastFailureAt.Equal(fail) {
		t.Fatalf("%+v", d)
	}
	if got := domain.Recipients(map[string]any{"to": []any{"a@example.com", 3}}); len(got) != 1 {
		t.Fatal(got)
	}
	if !domain.EventNotificationTestRequested.Known() {
		t.Fatal("extension event must be known")
	}
}
