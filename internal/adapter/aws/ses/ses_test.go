package ses_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/ses"
)

type fake struct {
	in  *sesv2.SendEmailInput
	err error
}

func (f *fake) SendEmail(_ context.Context, in *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	f.in = in
	return &sesv2.SendEmailOutput{}, f.err
}

func TestSend(t *testing.T) {
	f := &fake{}
	m := ses.NewWithAPI(f, "afs@example.com")
	if err := m.Send(t.Context(), []string{"a@example.com", "b@example.com"}, "subj\r\nBcc: x@example.com", "body"); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(f.in.Content.Simple.Subject.Data); got != "subj Bcc: x@example.com" || len(f.in.Destination.ToAddresses) != 2 || aws.ToString(f.in.FromEmailAddress) != "afs@example.com" {
		t.Fatalf("%q %+v", got, f.in)
	}
	if err := ses.NewWithAPI(f, "").Send(t.Context(), []string{"a@example.com"}, "s", "b"); err == nil {
		t.Fatal("no sender")
	}
	if err := m.Send(t.Context(), nil, "s", "b"); err == nil {
		t.Fatal("no recipients")
	}
	f.err = errors.New("MessageRejected")
	if err := m.Send(t.Context(), []string{"a@example.com"}, "s", "b"); err == nil {
		t.Fatal("want error")
	}
}
