package team_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// M4-B1 / RC-9: the team's service and own tags reach its roles and profiles, with the system tags and the
// record of applied keys; above the cap nothing is applied.
func TestTeamTagsAreAppliedToItsAWSResources(t *testing.T) {
	d := desired(idA, "payments", sonnet)
	d.Tags = map[string]string{"cost-center": "cc-42", "afs-team-id": "spoofed"}
	r := newRig(t, d)
	if _, err := r.c.Reconcile(t.Context(), ref(idA)); err != nil {
		t.Fatal(err)
	}
	name, err := team.TeamRoleName(idA)
	if err != nil {
		t.Fatal(err)
	}
	role, ok := r.iam.Role(name)
	if !ok {
		t.Fatal("no role")
	}
	if role.Tags["cost-center"] != "cc-42" || role.Tags[tags.KeyTeamID] != idA || role.Tags[tags.KeyTeamSlug] != "payments" {
		t.Errorf("role tags = %v", role.Tags)
	}
	if role.Tags[tags.KeyApplied] != "cost-center" {
		t.Errorf("applied record = %q", role.Tags[tags.KeyApplied])
	}
}

func TestTeamTagLimitIsRefusedBeforeAnyAWSCall(t *testing.T) {
	d := desired(idA, "payments", sonnet)
	d.Tags = map[string]string{}
	for i := range 40 {
		d.Tags[fmt.Sprintf("k%02d", i)] = "v"
	}
	r := newRig(t, d)
	out, err := r.c.Reconcile(t.Context(), ref(idA))
	if err == nil || engine.CodeOf(err) != tags.ErrorCode || !strings.Contains(err.Error(), "40") {
		t.Fatalf("err = %v (code %q)", err, engine.CodeOf(err))
	}
	if len(r.iam.Calls) != 0 || len(r.profiles.Calls) != 0 {
		t.Errorf("AWS was called: %v %v", r.iam.Calls, r.profiles.Calls)
	}
	if len(r.cluster.Envs) != 1 || r.cluster.Envs[0].TeamRoleARN == "" {
		t.Error("the cluster must still be applied with the computable team role ARN")
	}
	if _, ok := condition(out, tags.Condition); ok {
		t.Error("TagsApplied reported as applied")
	}
}
