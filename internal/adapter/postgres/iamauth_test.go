package postgres

import (
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// DP-1: with AFS_DB_IAM_AUTH the password of every new connection is a short-lived RDS IAM token
// signed locally for the connection string's host, port and user; nothing is stored or sent to AWS.
func TestIAMPassword(t *testing.T) {
	creds := credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret-for-tests-only", "")
	pw, err := IAMPassword("postgres://afs_app@db.example.internal:5432/afs?sslmode=require", "us-east-1", awssdk.CredentialsProvider(creds))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := pw(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"db.example.internal:5432?", "Action=connect", "DBUser=afs_app", "X-Amz-Signature="} {
		if !strings.Contains(tok, want) {
			t.Errorf("token lacks %q: %q", want, tok)
		}
	}
	if strings.Contains(tok, "secret-for-tests-only") {
		t.Error("token contains the secret key")
	}
}

func TestIAMPasswordRejectsUnusableURLs(t *testing.T) {
	creds := credentials.NewStaticCredentialsProvider("a", "b", "")
	for _, u := range []string{"::bad::", "postgres://db.example.internal/afs", "postgres://afs_app@/afs"} {
		if _, err := IAMPassword(u, "us-east-1", creds); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if _, err := IAMPassword("postgres://u@h:5432/d", "", creds); err == nil {
		t.Error("empty region accepted")
	}
}
