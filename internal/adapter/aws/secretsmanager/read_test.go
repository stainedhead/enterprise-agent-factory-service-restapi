package secretsmanager_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/secretsmanager"
)

type readFake struct {
	val *string
	err error
	id  string
}

func (f *readFake) GetSecretValue(_ context.Context, in *sm.GetSecretValueInput, _ ...func(*sm.Options)) (*sm.GetSecretValueOutput, error) {
	f.id = aws.ToString(in.SecretId)
	return &sm.GetSecretValueOutput{SecretString: f.val}, f.err
}

func TestReader(t *testing.T) {
	const arn = "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/t/x-AbCdEf"
	f := &readFake{val: aws.String("https://hooks.example.com/T/B/X")}
	r := secretsmanager.NewReaderWithAPI(f)
	c, err := r.Read(t.Context(), arn)
	if err != nil || c.Reveal() != "https://hooks.example.com/T/B/X" || f.id != arn {
		t.Fatal(c, err)
	}
	if s := c.String(); strings.Contains(s, "hooks") {
		t.Fatal("must print redacted")
	}
	for name, tc := range map[string]struct {
		arn string
		fk  *readFake
	}{
		"not an arn":    {"sec_123", f},
		"wrong service": {"arn:aws:s3:::bucket", f},
		"sdk error":     {arn, &readFake{err: errors.New("AccessDenied")}},
		"no text value": {arn, &readFake{}},
		"empty text":    {arn, &readFake{val: aws.String("")}},
	} {
		if _, err := secretsmanager.NewReaderWithAPI(tc.fk).Read(t.Context(), tc.arn); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
