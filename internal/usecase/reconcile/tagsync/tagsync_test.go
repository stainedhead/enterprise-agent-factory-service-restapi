package tagsync_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tagsync"
)

type fakeSource struct {
	d   tagsync.Desired
	err error
}

func (f fakeSource) LoadTagSync(context.Context, string) (tagsync.Desired, error) { return f.d, f.err }

// fakeTagger is an in-memory resource store; a missing id returns ErrMissing.
type fakeTagger struct {
	res     map[string]map[string]string
	tagged  []string
	untag   map[string][]string
	failTag error
	failGet error
}

func newTagger(res map[string]map[string]string) *fakeTagger {
	return &fakeTagger{res: res, untag: map[string][]string{}}
}

func (f *fakeTagger) Tags(_ context.Context, id string) (map[string]string, error) {
	if f.failGet != nil {
		return nil, f.failGet
	}
	t, ok := f.res[id]
	if !ok {
		return nil, tagsync.ErrMissing
	}
	out := map[string]string{}
	for k, v := range t {
		out[k] = v
	}
	return out, nil
}

func (f *fakeTagger) Tag(_ context.Context, id string, add map[string]string) error {
	if f.failTag != nil {
		return f.failTag
	}
	f.tagged = append(f.tagged, id)
	for k, v := range add {
		f.res[id][k] = v
	}
	return nil
}

func (f *fakeTagger) Untag(_ context.Context, id string, keys []string) error {
	f.untag[id] = append(f.untag[id], keys...)
	for _, k := range keys {
		delete(f.res[id], k)
	}
	return nil
}

const (
	secARN  = "arn:aws:secretsmanager:eu-west-1:111122223333:secret:afs/ns/_shared/db-AbCdEf"
	agtARN  = "arn:aws:secretsmanager:eu-west-1:111122223333:secret:afs/ns/bot/key-GhIjKl"
	queue1  = "https://sqs.eu-west-1.amazonaws.com/111122223333/afs-q1"
	queue2  = "https://sqs.eu-west-1.amazonaws.com/111122223333/afs-q2"
	teamRef = "team_01"
)

func queueURL(_, poolID string) (string, error) {
	return map[string]string{"pool_1": queue1, "pool_2": queue2}[poolID], nil
}

func desired() tagsync.Desired {
	return tagsync.Desired{
		TeamID: teamRef, Slug: "red", State: "active", Tags: map[string]string{"cost-center": "42", "env": "dev"},
		Secrets: []tagsync.Secret{{ARN: secARN}, {ARN: agtARN, AgentID: "agt_1", AgentName: "bot"}},
		Pools:   []tagsync.Pool{{ID: "pool_1", Name: "swarm"}, {ID: "pool_2", Name: "idle"}},
	}
}

func build(t *testing.T, src tagsync.Source, sm, q tagsync.Tagger) *tagsync.Controller {
	t.Helper()
	c, err := tagsync.NewController(tagsync.Config{Source: src, Secrets: sm, Queues: q, QueueURL: queueURL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func run(t *testing.T, c *tagsync.Controller) (engine.Outcome, error) {
	t.Helper()
	return c.Reconcile(context.Background(), engine.NewRef(tagsync.Kind, teamRef, teamRef))
}

func TestAppliesRequiredServiceTeamAndAutoTags(t *testing.T) {
	sm := newTagger(map[string]map[string]string{secARN: {"afs-team-id": teamRef}, agtARN: {"afs-team-id": teamRef, "afs-agent-id": "agt_1"}})
	q := newTagger(map[string]map[string]string{queue1: {"afs-team-id": teamRef}}) // pool_2 has no queue
	out, err := run(t, build(t, fakeSource{d: desired()}, sm, q))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{secARN, agtARN} {
		got := sm.res[id]
		for k, v := range map[string]string{"cost-center": "42", "env": "dev", "afs-team-id": teamRef, "afs-team-slug": "red", "afs-tag-keys": "cost-center,env"} {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q (%v)", id, k, got[k], v, got)
			}
		}
	}
	if a := sm.res[agtARN]; a["afs-agent-id"] != "agt_1" || a["afs-agent-name"] != "bot" {
		t.Errorf("agent secret tags %v", a)
	}
	if g := q.res[queue1]; g["afs-pool-id"] != "pool_1" || g["afs-pool-name"] != "swarm" || g["env"] != "dev" {
		t.Errorf("queue tags %v", g)
	}
	if _, ok := q.res[queue2]; ok {
		t.Error("a missing queue must not be created")
	}
	if len(out.Conditions) != 1 || out.Conditions[0].Type != tags.Condition || out.Conditions[0].Status != domain.ConditionTrue {
		t.Errorf("conditions %v", out.Conditions)
	}
	sort.Strings(out.Changed)
	if len(out.Changed) != 3 || !strings.HasPrefix(out.Changed[0], "Queue/") {
		t.Errorf("changed %v", out.Changed)
	}
}

func TestSecondPassChangesNothing(t *testing.T) {
	sm := newTagger(map[string]map[string]string{secARN: {}, agtARN: {}})
	q := newTagger(map[string]map[string]string{queue1: {}})
	c := build(t, fakeSource{d: desired()}, sm, q)
	if _, err := run(t, c); err != nil {
		t.Fatal(err)
	}
	sm.tagged, q.tagged = nil, nil
	out, err := run(t, c)
	if err != nil || len(out.Changed) != 0 || len(sm.tagged)+len(q.tagged) != 0 {
		t.Fatalf("out %+v err %v tagged %v %v", out, err, sm.tagged, q.tagged)
	}
}

func TestRemovesStaleTeamTagsOnly(t *testing.T) {
	// "old" was applied by AFS earlier (recorded in afs-tag-keys); "foreign" belongs to someone else.
	sm := newTagger(map[string]map[string]string{
		secARN: {"afs-team-id": teamRef, "old": "x", "foreign": "y", "afs-tag-keys": "old", "afs-orphan": "true"},
		agtARN: {},
	})
	q := newTagger(map[string]map[string]string{queue1: {"old": "x", "afs-tag-keys": "old"}})
	if _, err := run(t, build(t, fakeSource{d: desired()}, sm, q)); err != nil {
		t.Fatal(err)
	}
	if _, ok := sm.res[secARN]["old"]; ok {
		t.Error("stale team tag kept on the secret")
	}
	if sm.res[secARN]["foreign"] != "y" || sm.res[secARN]["afs-orphan"] != "true" {
		t.Errorf("a tag AFS does not own was removed: %v", sm.res[secARN])
	}
	if _, ok := q.res[queue1]["old"]; ok {
		t.Error("stale team tag kept on the queue")
	}
	if got := sm.untag[secARN]; len(got) != 1 || got[0] != "old" {
		t.Errorf("untagged %v", got)
	}
}

func TestTagLimitRefusedBeforeAnyCall(t *testing.T) {
	d := desired()
	d.Tags = map[string]string{}
	for i := range 45 {
		d.Tags[fmt.Sprintf("k%02d", i)] = "v"
	}
	sm := newTagger(map[string]map[string]string{secARN: {}, agtARN: {}})
	q := newTagger(map[string]map[string]string{queue1: {}})
	out, err := run(t, build(t, fakeSource{d: d}, sm, q))
	if err == nil || !errors.Is(err, engine.ErrTerminal) {
		t.Fatalf("err %v", err)
	}
	if len(sm.tagged)+len(q.tagged) != 0 || len(sm.res[secARN]) != 0 {
		t.Error("tags applied above the cap")
	}
	if len(out.Conditions) != 1 || out.Conditions[0].Status != domain.ConditionFalse || out.Conditions[0].Reason != tags.ErrorCode ||
		!strings.Contains(out.Conditions[0].Message, "limit is 40") {
		t.Errorf("conditions %+v", out.Conditions)
	}
}

func TestAWSFailureIsRetriedAndOthersStillSync(t *testing.T) {
	sm := newTagger(map[string]map[string]string{secARN: {}, agtARN: {}})
	sm.failTag = errors.New("throttled")
	q := newTagger(map[string]map[string]string{queue1: {}})
	_, err := run(t, build(t, fakeSource{d: desired()}, sm, q))
	if err == nil || errors.Is(err, engine.ErrTerminal) {
		t.Fatalf("err %v", err)
	}
	if q.res[queue1]["env"] != "dev" {
		t.Error("the queue was not synced after the secret failed")
	}
	var ce *engine.ConditionError
	if !errors.As(err, &ce) || ce.Type != tags.Condition {
		t.Errorf("condition error %v", err)
	}
}

func TestReadFailure(t *testing.T) {
	sm := newTagger(map[string]map[string]string{})
	sm.failGet = errors.New("denied")
	if _, err := run(t, build(t, fakeSource{d: desired()}, sm, newTagger(nil))); err == nil {
		t.Fatal("want error")
	}
}

func TestStatesAndNotFound(t *testing.T) {
	for _, st := range []string{"deleting", "deleted"} {
		d := desired()
		d.State = st
		sm := newTagger(map[string]map[string]string{secARN: {}})
		out, err := run(t, build(t, fakeSource{d: d}, sm, newTagger(nil)))
		if err != nil || !out.NoStatus || len(sm.tagged) != 0 {
			t.Errorf("%s: %+v %v", st, out, err)
		}
	}
	out, err := run(t, build(t, fakeSource{err: domain.NotFound("team")}, newTagger(nil), newTagger(nil)))
	if err != nil || !out.NoStatus {
		t.Errorf("not found: %+v %v", out, err)
	}
	boom := errors.New("db down")
	if _, err := run(t, build(t, fakeSource{err: boom}, newTagger(nil), newTagger(nil))); !errors.Is(err, boom) {
		t.Errorf("err %v", err)
	}
	// A suspended team's resources are still tagged.
	d := desired()
	d.State = "suspended"
	sm := newTagger(map[string]map[string]string{secARN: {}, agtARN: {}})
	if _, err := run(t, build(t, fakeSource{d: d}, sm, newTagger(nil))); err != nil || sm.res[secARN]["env"] != "dev" {
		t.Errorf("suspended: %v %v", err, sm.res[secARN])
	}
}

func TestQueueURLError(t *testing.T) {
	c, _ := tagsync.NewController(tagsync.Config{Source: fakeSource{d: desired()}, Secrets: newTagger(map[string]map[string]string{secARN: {}, agtARN: {}}),
		Queues: newTagger(nil), QueueURL: func(string, string) (string, error) { return "", errors.New("bad id") }})
	if _, err := run(t, c); err == nil {
		t.Fatal("want error")
	}
}

func TestNewControllerValidates(t *testing.T) {
	if _, err := tagsync.NewController(tagsync.Config{}); err == nil {
		t.Fatal("want error")
	}
}

func TestRoutes(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range tagsync.Routes() {
		seen[r.Table] = true
		if len(r.Targets) == 0 && len(r.FanOut) == 0 {
			t.Errorf("route %s reaches nothing", r.Table)
		}
	}
	for _, tb := range []string{"team_tags", "secrets", "agent_pools", "agents", "service_tags"} {
		if !seen[tb] {
			t.Errorf("no route for %s", tb)
		}
	}
	if _, err := engine.NewRegistry(tagsync.Routes()...); err != nil {
		t.Fatal(err)
	}
}

// FR-R13: the refused-tags condition names resources by ARN; the stored message carries neither the
// ARN nor the account id, and is bounded.
func TestRefusedConditionMessageIsSanitised(t *testing.T) {
	d := desired()
	d.Tags = map[string]string{}
	for i := range 45 {
		d.Tags[fmt.Sprintf("k%02d", i)] = "v"
	}
	sm := newTagger(map[string]map[string]string{secARN: {}, agtARN: {}})
	out, err := run(t, build(t, fakeSource{d: d}, sm, newTagger(map[string]map[string]string{queue1: {}})))
	if err == nil || len(out.Conditions) != 1 {
		t.Fatalf("err %v conditions %+v", err, out.Conditions)
	}
	msg := out.Conditions[0].Message
	if strings.Contains(msg, "111122223333") || strings.Contains(msg, "arn:aws") || len(msg) > 512 {
		t.Errorf("message not sanitised: %q", msg)
	}
	if !strings.Contains(msg, "limit is 40") {
		t.Errorf("message lost its meaning: %q", msg)
	}
	// the returned error keeps the full text for the reconciler log
	if !strings.Contains(err.Error(), "111122223333") {
		t.Errorf("full error should reach the log, got %q", err.Error())
	}
}
