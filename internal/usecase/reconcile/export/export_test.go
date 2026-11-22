package export_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/export"
)

var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

type fakeStore struct {
	pending   []export.Request
	bundle    domain.Bundle
	buildErr  error
	done      map[string]export.Result
	failed    map[string]string
	claimErr  error
	succeedEr error
}

func (s *fakeStore) Claim(context.Context) (*export.Request, error) {
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	if len(s.pending) == 0 {
		return nil, nil
	}
	r := s.pending[0]
	s.pending = s.pending[1:]
	return &r, nil
}

func (s *fakeStore) Build(context.Context, []string, time.Time) (domain.Bundle, error) {
	return s.bundle, s.buildErr
}

func (s *fakeStore) Succeed(_ context.Context, id string, r export.Result) error {
	if s.succeedEr != nil {
		return s.succeedEr
	}
	s.done[id] = r
	return nil
}

func (s *fakeStore) Fail(_ context.Context, id, sum string) error { s.failed[id] = sum; return nil }

type fakeBlob struct {
	objects map[string][]byte
	putErr  error
	signErr error
	ttl     time.Duration
}

func (b *fakeBlob) Put(_ context.Context, key string, body []byte) (string, error) {
	if b.putErr != nil {
		return "", b.putErr
	}
	b.objects[key] = body
	return "s3://exports-bucket/" + key, nil
}

func (b *fakeBlob) PresignGet(_ context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	b.ttl = ttl
	if b.signErr != nil {
		return "", time.Time{}, b.signErr
	}
	return "https://exports-bucket.s3.example/" + key + "?sig=EXAMPLE", now.Add(ttl), nil
}

func rig() (*export.Builder, *fakeStore, *fakeBlob) {
	st := &fakeStore{done: map[string]export.Result{}, failed: map[string]string{},
		bundle: domain.Bundle{SchemaVersion: domain.BundleSchemaVersion, ExportedAt: now.Format(time.RFC3339), ExternalSystems: []domain.Row{},
			Teams: []domain.TeamBundle{{Team: domain.Row{"slug": "payments"}}}},
		pending: []export.Request{{ID: "exp_1"}, {ID: "exp_2"}}}
	bl := &fakeBlob{objects: map[string][]byte{}}
	return &export.Builder{Store: st, Blob: bl, Clock: func() time.Time { return now }}, st, bl
}

func TestRunBuildsEveryPendingExport(t *testing.T) {
	b, st, bl := rig()
	if err := b.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(st.done) != 2 || len(st.failed) != 0 {
		t.Fatalf("done %v failed %v", st.done, st.failed)
	}
	r := st.done["exp_1"]
	doc := bl.objects[export.Key("exp_1")]
	bundle, err := domain.ParseBundle(doc)
	if err != nil {
		t.Fatalf("the stored object does not verify: %v", err)
	}
	if r.SHA256 != bundle.SHA256 || r.SizeBytes != int64(len(doc)) || r.S3URI != "s3://exports-bucket/exports/exp_1.json" || r.Teams != 1 {
		t.Fatalf("result %+v", r)
	}
	if bl.ttl != 7*24*time.Hour || !r.ExpiresAt.Equal(now.Add(7*24*time.Hour)) || !strings.Contains(r.DownloadURL, "exp_1") {
		t.Fatalf("link %v %v", bl.ttl, r.ExpiresAt)
	}
}

func TestFailuresAreRecordedWithoutChecksum(t *testing.T) {
	for name, mod := range map[string]func(*fakeStore, *fakeBlob){
		"read": func(s *fakeStore, _ *fakeBlob) { s.buildErr = errors.New("connection reset") },
		"put":  func(_ *fakeStore, b *fakeBlob) { b.putErr = errors.New("AccessDenied: arn:aws:kms:secret") },
		"sign": func(_ *fakeStore, b *fakeBlob) { b.signErr = errors.New("boom") },
		"seal": func(s *fakeStore, _ *fakeBlob) {
			s.bundle.Teams[0].Secrets = []domain.Row{{"password": "x"}}
		},
	} {
		b, st, bl := rig()
		mod(st, bl)
		if err := b.Run(t.Context()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(st.done) != 0 || len(st.failed) != 2 {
			t.Errorf("%s: done %v failed %v", name, st.done, st.failed)
		}
		for _, sum := range st.failed {
			if strings.Contains(sum, "arn:") || strings.Contains(sum, "connection") || !strings.HasSuffix(sum, " failed") {
				t.Errorf("%s: summary leaks or is malformed: %q", name, sum)
			}
		}
	}
}

func TestStoreErrorsAreReturned(t *testing.T) {
	b, st, _ := rig()
	st.claimErr = errors.New("db down")
	if err := b.Run(t.Context()); err == nil {
		t.Fatal("claim error swallowed")
	}
	b, st, _ = rig()
	st.succeedEr = errors.New("db down")
	if err := b.Run(t.Context()); err == nil {
		t.Fatal("succeed error swallowed")
	}
}

type sched struct{ names []string }

func (s *sched) Every(n string, _ time.Duration, _ func(context.Context) error) {
	s.names = append(s.names, n)
}

func TestRegister(t *testing.T) {
	b, _, _ := rig()
	s := &sched{}
	b.Register(s)
	if len(s.names) != 1 {
		t.Fatal(s.names)
	}
}
