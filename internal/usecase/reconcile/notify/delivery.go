package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Message is what a channel is asked to deliver.
type Message struct {
	ChannelID   string
	ChannelName string
	TeamID      string
	EventID     string
	EventType   string
	Subject     string
	Text        string
	Detail      json.RawMessage
}

// HTTPSender delivers a message to Slack, a webhook or Jira with the channel's credential. It is the
// only code that calls a team-supplied URL from the reconciler; implementations refuse private
// addresses (notifyhttp).
type HTTPSender interface {
	Send(ctx context.Context, ch Channel, cred Credential, m Message) error
}

// Mailer sends an email through SES.
type Mailer interface {
	Send(ctx context.Context, to []string, subject, body string) error
}

// BusTester puts one test event on an EventBridge channel's target bus.
type BusTester interface {
	PutTest(ctx context.Context, busARN string, m Message) error
}

// Deliverer sends a test message by channel kind.
type Deliverer struct {
	HTTP HTTPSender
	Mail Mailer
	Bus  BusTester
}

// Deliver sends m on ch.
func (d Deliverer) Deliver(ctx context.Context, ch Channel, cred Credential, m Message) error {
	switch ch.Kind {
	case domain.ChannelSlack, domain.ChannelWebhook, domain.ChannelJira:
		if d.HTTP == nil {
			return errors.New("delivery to " + string(ch.Kind) + " is not configured")
		}
		return d.HTTP.Send(ctx, ch, cred, m)
	case domain.ChannelEmail:
		if d.Mail == nil {
			return errors.New("email delivery is not configured")
		}
		return d.Mail.Send(ctx, domain.Recipients(ch.Config), m.Subject, m.Text)
	case domain.ChannelEventBridge:
		if d.Bus == nil {
			return errors.New("event bus delivery is not configured")
		}
		return d.Bus.PutTest(ctx, domain.ConfigString(ch.Config, "eventBusArn"), m)
	}
	return fmt.Errorf("unknown channel kind %q", ch.Kind)
}

// Delivery is one observed outcome on a channel.
type Delivery struct {
	At  time.Time
	OK  bool
	Err string
	// TestID is set when the outcome is the result of a test request.
	TestID string
}

// Recorder merges an outcome into a channel's status.
type Recorder interface {
	Record(ctx context.Context, ch Channel, d Delivery) error
}

// Events emits notification.delivery_failed.
type Events interface {
	DeliveryFailed(ctx context.Context, ch Channel, errText, eventType string) error
}

// ApplyDelivery returns the conditions (DeliveryHealthy and DeliveryDetail) that result from an
// outcome on top of the stored ones. The error text is stripped of URLs and cut.
func ApplyDelivery(prev []domain.Condition, d Delivery) []domain.Condition {
	detail := domain.DeliveryDetailOf(prev)
	at := d.At.UTC()
	errText := RedactURLs(d.Err)
	health := cond(domain.CondDeliveryHealthy, domain.ConditionTrue, domain.DeliveryDelivered, "")
	if d.TestID != "" {
		detail.LastTestID = d.TestID
		ok := d.OK
		detail.TestDelivered = &ok
		detail.TestError = ""
		if !d.OK {
			detail.TestError = errText
		}
	}
	if d.OK {
		detail.LastDeliveredAt = &at
	} else {
		detail.LastFailureAt = &at
		detail.LastError = errText
		reason := domain.DeliveryFailed
		if d.TestID != "" {
			reason = domain.DeliveryTestFailed
		}
		health = cond(domain.CondDeliveryHealthy, domain.ConditionFalse, reason, errText)
	}
	health.LastTransitionTime = at
	return []domain.Condition{health, detail.Condition(at)}
}

// TestMessage is the message a test sends.
func TestMessage(ch Channel, testID string, at time.Time) Message {
	text := fmt.Sprintf("This is a test notification from the Enterprise Agent Factory Service for channel %q (test %s, %s).", ch.Name, testID, at.UTC().Format(time.RFC3339))
	detail, _ := json.Marshal(map[string]any{"channelId": ch.ID, "testId": testID})
	return Message{ChannelID: ch.ID, ChannelName: ch.Name, TeamID: ch.TeamID, EventID: testID, EventType: string(domain.EventNotificationTestRequested),
		Subject: "[AFS] Test notification", Text: text, Detail: detail}
}
