package agent_test

import (
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent/agenttest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team/teamtest"
)

// M4-A4 / RC-US-4.1: a dedicated agent's role may assume each linked role by its exact ARN (disabled links
// excluded), and trusts the reconciler's role so links can be verified. No wildcard is granted.
func TestDedicatedRoleAssumesLinkedRolesByExactARN(t *testing.T) {
	d := desired()
	d.IAMIdentity = agent.IdentityDedicated
	d.RoleLinks = []agent.RoleLink{
		{ProfileName: "prod", RoleARN: "arn:aws:iam::444455556666:role/deployer", ExternalID: "afs-a", Status: "verified"},
		{ProfileName: "off", RoleARN: "arn:aws:iam::777788889999:role/old", ExternalID: "afs-b", Status: "disabled"},
	}
	iam := teamtest.NewIAM()
	vols := agenttest.NewVolumes()
	vols.States[d.ID] = agent.VolumeState{Exists: true, Bound: true, RequestedGiB: d.Volume.SizeGiB, CapacityGiB: d.Volume.SizeGiB}
	ctl, err := agent.NewController(agent.Config{Source: agenttest.NewSource(d), Cluster: &agenttest.Cluster{}, IAM: iam,
		PodIdentity: agenttest.NewPodIdentities(), Volumes: vols, Snapshots: &agenttest.Snapshots{Latest: map[string]string{}},
		AWS: awsCfg(), LinkVerifierARN: "arn:aws:iam::111122223333:role/afs-reconciler"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.Reconcile(t.Context(), ref()); err != nil {
		t.Fatal(err)
	}
	spec, ok := iam.Role("afs-01JD0000000000000000000021")
	if !ok {
		t.Fatal("no role")
	}
	pol := spec.Policies["agent"]
	if !strings.Contains(pol, "AssumeLinkedRoles") || !strings.Contains(pol, "arn:aws:iam::444455556666:role/deployer") {
		t.Errorf("policy lacks the link: %s", pol)
	}
	if strings.Contains(pol, "777788889999") {
		t.Errorf("a disabled link must not be assumable: %s", pol)
	}
	if !strings.Contains(spec.TrustPolicy, "arn:aws:iam::111122223333:role/afs-reconciler") {
		t.Errorf("trust policy lacks the verifier: %s", spec.TrustPolicy)
	}
}
