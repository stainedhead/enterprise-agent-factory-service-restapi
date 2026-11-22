// Package ses is the SES adapter of the reconciler's email notifications.
package ses

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

// API is the subset of the SES v2 SDK client this package calls.
type API interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, opts ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// Mailer implements notify.Mailer: plain-text messages from one verified sender address.
type Mailer struct {
	api  API
	from string
}

var _ notify.Mailer = (*Mailer)(nil)

// New returns a Mailer on a configured SDK config; from is the verified sender (AFS_NOTIFY_EMAIL_FROM).
func New(cfg aws.Config, from string) *Mailer {
	return &Mailer{api: sesv2.NewFromConfig(cfg), from: from}
}

// NewWithAPI returns a Mailer on api (tests).
func NewWithAPI(api API, from string) *Mailer { return &Mailer{api: api, from: from} }

// Send implements notify.Mailer.
func (m *Mailer) Send(ctx context.Context, to []string, subject, body string) error {
	if m.from == "" {
		return errors.New("ses: the sender address is not configured (AFS_NOTIFY_EMAIL_FROM)")
	}
	if len(to) == 0 {
		return errors.New("ses: no recipients")
	}
	if strings.ContainsAny(subject, "\r\n") {
		subject = strings.Join(strings.Fields(subject), " ")
	}
	_, err := m.api.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(m.from),
		Destination:      &types.Destination{ToAddresses: to},
		Content: &types.EmailContent{Simple: &types.Message{
			Subject: &types.Content{Data: aws.String(subject), Charset: aws.String("UTF-8")},
			Body:    &types.Body{Text: &types.Content{Data: aws.String(body), Charset: aws.String("UTF-8")}},
		}},
	})
	if err != nil {
		return fmt.Errorf("ses: SendEmail: %w", err)
	}
	return nil
}
