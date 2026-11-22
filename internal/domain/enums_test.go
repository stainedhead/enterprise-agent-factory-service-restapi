package domain_test

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

func TestEnumValid(t *testing.T) {
	checks := map[string]func(string) bool{
		"TeamKind":            func(s string) bool { return domain.TeamKind(s).Valid() },
		"TeamState":           func(s string) bool { return domain.TeamState(s).Valid() },
		"IsolationTier":       func(s string) bool { return domain.IsolationTier(s).Valid() },
		"TeardownRule":        func(s string) bool { return domain.TeardownRule(s).Valid() },
		"TeardownReason":      func(s string) bool { return domain.TeardownReason(s).Valid() },
		"TeamRole":            func(s string) bool { return domain.TeamRole(s).Valid() },
		"SubjectType":         func(s string) bool { return domain.SubjectType(s).Valid() },
		"PrincipalType":       func(s string) bool { return domain.PrincipalType(s).Valid() },
		"SecretDelivery":      func(s string) bool { return domain.SecretDelivery(s).Valid() },
		"IAMIdentity":         func(s string) bool { return domain.IAMIdentity(s).Valid() },
		"VolumeOnDelete":      func(s string) bool { return domain.VolumeOnDelete(s).Valid() },
		"SnapshotSchedule":    func(s string) bool { return domain.SnapshotSchedule(s).Valid() },
		"RoleLinkStatus":      func(s string) bool { return domain.RoleLinkStatus(s).Valid() },
		"PoolState":           func(s string) bool { return domain.PoolState(s).Valid() },
		"SharedVolume":        func(s string) bool { return domain.SharedVolume(s).Valid() },
		"WorkOrderKind":       func(s string) bool { return domain.WorkOrderKind(s).Valid() },
		"Severity":            func(s string) bool { return domain.Severity(s).Valid() },
		"WorkOrderStatus":     func(s string) bool { return domain.WorkOrderStatus(s).Valid() },
		"SLAState":            func(s string) bool { return domain.SLAState(s).Valid() },
		"WorkOrderSource":     func(s string) bool { return domain.WorkOrderSource(s).Valid() },
		"RepoStatus":          func(s string) bool { return domain.RepoStatus(s).Valid() },
		"OwnerSource":         func(s string) bool { return domain.OwnerSource(s).Valid() },
		"ReportKind":          func(s string) bool { return domain.ReportKind(s).Valid() },
		"Cadence":             func(s string) bool { return domain.Cadence(s).Valid() },
		"ReportFormat":        func(s string) bool { return domain.ReportFormat(s).Valid() },
		"TagSource":           func(s string) bool { return domain.TagSource(s).Valid() },
		"BudgetPeriod":        func(s string) bool { return domain.BudgetPeriod(s).Valid() },
		"OnExhaust":           func(s string) bool { return domain.OnExhaust(s).Valid() },
		"IdPKind":             func(s string) bool { return domain.IdPKind(s).Valid() },
		"ExportStatus":        func(s string) bool { return domain.ExportStatus(s).Valid() },
		"PriceProposalStatus": func(s string) bool { return domain.PriceProposalStatus(s).Valid() },
		"CostTagAWSStatus":    func(s string) bool { return domain.CostTagAWSStatus(s).Valid() },
		"SecretScope":         func(s string) bool { return domain.SecretScope(s).Valid() },
		"ConfigSource":        func(s string) bool { return domain.ConfigSource(s).Valid() },
		"ExtSystemScope":      func(s string) bool { return domain.ExtSystemScope(s).Valid() },
		"ExtSystemTeamState":  func(s string) bool { return domain.ExtSystemTeamState(s).Valid() },
		"ExtSystemDisabledAt": func(s string) bool { return domain.ExtSystemDisabledAt(s).Valid() },
		"ServiceTagMode":      func(s string) bool { return domain.ServiceTagMode(s).Valid() },
		"ImportResult":        func(s string) bool { return domain.ImportResult(s).Valid() },
		"SwarmTemplateKind":   func(s string) bool { return domain.SwarmTemplateKind(s).Valid() },
		"ActorType":           func(s string) bool { return domain.ActorType(s).Valid() },
		"AuditOutcome":        func(s string) bool { return domain.AuditOutcome(s).Valid() },
	}
	if len(checks) != len(domain.EnumValues) {
		t.Fatalf("checks = %d, enums = %d", len(checks), len(domain.EnumValues))
	}
	for name, vs := range domain.EnumValues {
		for _, v := range vs {
			if !checks[name](v) {
				t.Errorf("%s(%q) invalid", name, v)
			}
		}
		if checks[name]("not-a-value") {
			t.Errorf("%s accepts an unknown value", name)
		}
	}
}
