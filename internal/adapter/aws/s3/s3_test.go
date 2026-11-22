package s3_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/awstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/s3"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	bucket = "example-afs-reports"
	kmsKey = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"
)

func newStore(t *testing.T) (*s3.Store, *awstest.Server) {
	t.Helper()
	srv := awstest.New(t)
	st, err := s3.New(srv.Config(), s3.Options{Bucket: bucket, KMSKeyID: kmsKey, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	return st, srv
}

func xml(status int, body string) awstest.Response {
	return awstest.Response{Status: status, Body: body, ContentType: "application/xml"}
}

func TestNewRequiresABucket(t *testing.T) {
	if _, err := s3.New(awstest.New(t).Config(), s3.Options{}); err == nil {
		t.Fatal("a store without a bucket must not be built")
	}
}

func TestPutEncryptsWithKMS(t *testing.T) {
	st, srv := newStore(t)
	srv.OnAction("PUT /"+bucket+"/reports/team_x/rsc_y/usage.csv", xml(200, ""))
	res, err := st.Put(context.Background(), port.PutObject{Key: "reports/team_x/rsc_y/usage.csv", Body: []byte("agent,calls\nagt_1,3\n"), ContentType: "text/csv"})
	if err != nil {
		t.Fatal(err)
	}
	if res.URI != "s3://"+bucket+"/reports/team_x/rsc_y/usage.csv" || res.Size != 20 {
		t.Errorf("result = %+v", res)
	}
	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if got := r.Header.Get("X-Amz-Server-Side-Encryption"); got != "aws:kms" {
		t.Errorf("SSE = %q, want aws:kms", got)
	}
	if got := r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"); got != kmsKey {
		t.Errorf("KMS key = %q", got)
	}
	if got := r.Header.Get("Content-Type"); got != "text/csv" {
		t.Errorf("content type = %q", got)
	}
	if !strings.Contains(string(r.Body), "agent,calls\nagt_1,3\n") {
		t.Errorf("body = %q", r.Body)
	}
}

func TestPutWithoutAKeyUsesTheManagedKey(t *testing.T) {
	srv := awstest.New(t)
	st, err := s3.New(srv.Config(), s3.Options{Bucket: bucket, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	srv.OnAction("PUT /"+bucket+"/a.csv", xml(200, ""))
	if _, err := st.Put(context.Background(), port.PutObject{Key: "a.csv", Body: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	r := srv.Requests()[0]
	if r.Header.Get("X-Amz-Server-Side-Encryption") != "aws:kms" || r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != "" {
		t.Errorf("headers = %v", r.Header)
	}
}

func TestPutErrors(t *testing.T) {
	st, srv := newStore(t)
	srv.OnAction("PUT /"+bucket+"/denied.csv", xml(403, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>r1</RequestId></Error>`))
	_, err := st.Put(context.Background(), port.PutObject{Key: "denied.csv", Body: []byte("x")})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || !strings.Contains(err.Error(), "s3://"+bucket+"/denied.csv") {
		t.Fatalf("err = %v", err)
	}
	for _, key := range []string{"", "/abs", "a/../b", strings.Repeat("k", 1025)} {
		before := len(srv.Requests())
		if _, err := st.Put(context.Background(), port.PutObject{Key: key, Body: []byte("x")}); err == nil {
			t.Errorf("key %q was accepted", key)
		}
		if len(srv.Requests()) != before {
			t.Errorf("key %q reached S3", key)
		}
	}
}

func TestPresignGet(t *testing.T) {
	st, srv := newStore(t)
	before := time.Now().UTC()
	got, err := st.PresignGet(context.Background(), "reports/team_x/rsc_y/usage.csv", 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/"+bucket+"/reports/team_x/rsc_y/usage.csv" {
		t.Errorf("path = %s", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Expires") != "604800" || q.Get("X-Amz-Signature") == "" || !strings.HasPrefix(q.Get("X-Amz-Algorithm"), "AWS4-HMAC-SHA256") {
		t.Errorf("query = %v", q)
	}
	if d := got.ExpiresAt.Sub(before); d < 7*24*time.Hour || d > 7*24*time.Hour+time.Minute {
		t.Errorf("expires at %s, %s from signing", got.ExpiresAt, d)
	}
	if len(srv.Requests()) != 0 {
		t.Error("presigning must not call S3")
	}
	for _, d := range []time.Duration{0, -time.Second, 7*24*time.Hour + time.Second} {
		if _, err := st.PresignGet(context.Background(), "a.csv", d); err == nil {
			t.Errorf("expiry %s was accepted", d)
		}
	}
	if _, err := st.PresignGet(context.Background(), "", time.Hour); err == nil {
		t.Error("an empty key was accepted")
	}
}

func TestListFollowsPages(t *testing.T) {
	st, srv := newStore(t)
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Method != "GET" || r.Path != "/"+bucket {
			return nil
		}
		if r.Query.Get("prefix") != "reports/team_x/" {
			t.Errorf("prefix = %q", r.Query.Get("prefix"))
		}
		if r.Query.Get("continuation-token") == "" {
			resp := xml(200, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>tok2</NextContinuationToken>
<Contents><Key>reports/team_x/a.csv</Key><Size>10</Size><LastModified>2026-11-22T06:00:00.000Z</LastModified></Contents></ListBucketResult>`)
			return &resp
		}
		if r.Query.Get("continuation-token") != "tok2" {
			t.Errorf("token = %q", r.Query.Get("continuation-token"))
		}
		resp := xml(200, `<ListBucketResult><IsTruncated>false</IsTruncated>
<Contents><Key>reports/team_x/b.csv</Key><Size>20</Size><LastModified>2026-11-23T06:00:00.000Z</LastModified></Contents></ListBucketResult>`)
		return &resp
	})
	got, err := st.List(context.Background(), "reports/team_x/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "reports/team_x/a.csv" || got[0].Size != 10 || got[1].Key != "reports/team_x/b.csv" ||
		!got[1].LastModified.Equal(time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("objects = %+v", got)
	}
}

func TestListEmptyAndFailure(t *testing.T) {
	st, srv := newStore(t)
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Query.Get("prefix") == "boom/" {
			resp := xml(500, `<Error><Code>InternalError</Code><Message>try again</Message></Error>`)
			return &resp
		}
		resp := xml(200, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
		return &resp
	})
	got, err := st.List(context.Background(), "none/")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list = %v %v", got, err)
	}
	if _, err := st.List(context.Background(), "boom/"); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
