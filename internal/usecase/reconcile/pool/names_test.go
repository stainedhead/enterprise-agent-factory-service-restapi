package pool_test

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

func TestNames(t *testing.T) {
	tests := []struct{ name, got, want string }{
		{"workload", pool.WorkloadName("fixer"), "pool-fixer"},
		{"service account", pool.ServiceAccountName("fixer"), "pool-fixer"},
		{"config map", pool.ConfigMapName("fixer"), "pool-fixer-config"},
		{"aws config map", pool.AWSConfigMapName("fixer"), "pool-fixer-aws"},
		{"env secret", pool.EnvSecretName("fixer"), "pool-fixer-env"},
		{"files secret", pool.FilesSecretName("fixer"), "pool-fixer-files"},
		{"shared claim", pool.SharedClaimName("fixer"), "pool-fixer-shared"},
		{"egress policy", pool.EgressPolicyName("pool_01JD0000000000000000000031"), "afs-egress-pool-01jd0000000000000000000031"},
		{"handoff variable", pool.HandoffEnvName("fixer"), "AFS_HANDOFF_FIXER_QUEUE_URL"},
		{"handoff variable with hyphens", pool.HandoffEnvName("cve-fixer-2"), "AFS_HANDOFF_CVE_FIXER_2_QUEUE_URL"},
		{"inbox variable", pool.InboxEnvName, "AFS_INBOX_QUEUE_URL"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
	role, err := pool.RoleName(poolA)
	if err != nil || role != "afs-01JD0000000000000000000031" {
		t.Errorf("role = %q, %v", role, err)
	}
	if _, err := pool.RoleName("nope"); err == nil {
		t.Error("role name of a bad id accepted")
	}
}

func TestQueueName(t *testing.T) {
	re := regexp.MustCompile(`^afs-[0-9A-Z]{26}-[0-9a-f]{8}$`)
	n1, err := pool.QueueName(teamID, poolA)
	if err != nil || !re.MatchString(n1) || len(n1) > 80 {
		t.Fatalf("name = %q, %v", n1, err)
	}
	n2, _ := pool.QueueName(teamID, poolB)
	n3, _ := pool.QueueName(teamID, poolA)
	if n1 == n2 || n1 != n3 {
		t.Errorf("names are not deterministic per pool: %s %s %s", n1, n2, n3)
	}
	if _, err := pool.QueueName("team", poolA); err == nil {
		t.Error("a team id without a prefix accepted")
	}
	if _, err := pool.QueueName(teamID, ""); err == nil {
		t.Error("an empty pool id accepted")
	}
}

func TestWithQueueStatements(t *testing.T) {
	const base = `{"Version":"2012-10-17","Statement":[{"Sid":"X","Effect":"Allow","Action":["a:b"],"Resource":["*"]}]}`
	got, err := pool.WithQueueStatements(base, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Statement []map[string]any }
	if err := json.Unmarshal([]byte(got), &doc); err != nil || len(doc.Statement) != 1 {
		t.Errorf("no queues must add nothing: %s %v", got, err)
	}
	got, err = pool.WithQueueStatements(base, "arn:aws:sqs:us-east-1:111122223333:in", []string{"arn:aws:sqs:us-east-1:111122223333:z", "arn:aws:sqs:us-east-1:111122223333:a", "arn:aws:sqs:us-east-1:111122223333:z", ""})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"Resource":["arn:aws:sqs:us-east-1:111122223333:a","arn:aws:sqs:us-east-1:111122223333:z"]`) || strings.Count(got, "SendHandoffs") != 1 {
		t.Errorf("send statement = %s", got)
	}
	if _, err := pool.WithQueueStatements("not json", "", nil); err == nil {
		t.Error("a policy that is not JSON accepted")
	}
}

func TestDedicatedTierNodePool(t *testing.T) {
	d := desired(poolA, "fixer")
	d.Team.IsolationTier = agent.TierDedicated
	r := newRig(t, pool.Hooks{}, d)
	if _, err := r.ctl.Reconcile(t.Context(), ref(poolA)); err != nil {
		t.Fatal(err)
	}
	np := r.cl.Last().NodePool
	if !np.Dedicated || np.Tags["afs-team-id"] != teamID || np.Tags["afs-pool-id"] != "" || np.Tags["afs-pool-name"] != "" || np.Tags["cost-center"] != "cc-1" {
		t.Errorf("node pool = %+v; it is the team's, so it carries the team's tags and no pool's", np)
	}
	if shared := newRig(t, pool.Hooks{}, desired(poolA, "fixer")); true {
		_, _ = shared.ctl.Reconcile(t.Context(), ref(poolA))
		if shared.cl.Last().NodePool.Dedicated {
			t.Error("a shared-tier pool got dedicated nodes")
		}
	}
}

func TestBadIDsAreReported(t *testing.T) {
	t.Run("team id without prefix", func(t *testing.T) {
		d := desired(poolB, "reviewer")
		d.Team.ID = "teamless"
		d.ReceivesFrom = []pool.Peer{{ID: poolA, Name: "fixer"}}
		r := newRig(t, pool.Hooks{}, d)
		_, err := r.ctl.Reconcile(t.Context(), ref(poolB))
		var ce *engine.ConditionError
		if !errors.As(err, &ce) || ce.Type != domain.CondIamReady {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("pool id without prefix", func(t *testing.T) {
		d := desired("nope", "reviewer")
		r := newRig(t, pool.Hooks{}, d)
		if _, err := r.ctl.Reconcile(t.Context(), ref("nope")); err == nil {
			t.Error("a pool id without a prefix was reconciled")
		}
	})
	t.Run("sender id without prefix", func(t *testing.T) {
		d := desired(poolB, "reviewer")
		d.ReceivesFrom = []pool.Peer{{ID: "nope", Name: "fixer"}}
		r := newRig(t, pool.Hooks{}, d)
		if _, err := r.ctl.Reconcile(t.Context(), ref(poolB)); err == nil {
			t.Error("a sender id without a prefix was reconciled")
		}
	})
}

func TestAllowedSummaryMessages(t *testing.T) {
	d := desired(poolA, "fixer")
	r := newRig(t, pool.Hooks{}, d)
	out, err := r.ctl.Reconcile(t.Context(), ref(poolA))
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := cond(out, "EgressPolicyReady"); !strings.Contains(c.Message, "no external system") {
		t.Errorf("message = %q", c.Message)
	}
}
