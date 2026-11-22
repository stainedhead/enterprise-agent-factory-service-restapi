package matrix

import (
	"sort"
	"testing"
)

// contractOutboxExempt is the embedded copy of the root contract's CT-API-8 exemption list
// (docs/contracts/api-contract.md, "Mutations without an outbox event", CD-7): the mutations that have no
// event type in the contract's event table. The value is the contract's category. Change this list and
// the contract together; a divergence fails TestNoOutboxMatchesContractExemptions.
var contractOutboxExempt = map[string]string{
	"AddMember":                        "members",
	"AddSecurityOperator":              "members",
	"ConnectAgentToCollaboration":      "collaboration",
	"CreateAgentIdentity":              "config",
	"CreateAgentRoleLink":              "config",
	"CreateAgentSnapshot":              "config",
	"CreateApiKey":                     "api-keys",
	"CreateCatalogSystem":              "catalog",
	"CreateExport":                     "config",
	"CreateIdentityProvider":           "settings",
	"CreateLogDestination":             "settings",
	"CreateModel":                      "catalog",
	"CreateModelPrice":                 "catalog",
	"CreateNotificationChannel":        "settings",
	"CreatePoolRoleLink":               "config",
	"CreateReportSchedule":             "schedules",
	"CreateServiceApiKey":              "api-keys",
	"CreateServiceNotificationChannel": "settings",
	"CreateStage":                      "catalog",
	"CreateSwarmTemplate":              "swarm-templates",
	"DeclareTeamExternalSystem":        "catalog",
	"DeleteAgentConfigValue":           "config",
	"DisconnectAgentFromCollaboration": "collaboration",
	"DeleteAgentIdentity":              "config",
	"DeleteAgentRoleLink":              "config",
	"DeleteAgentTemplate":              "catalog",
	"DeleteApiKey":                     "api-keys",
	"DeleteCatalogSystem":              "catalog",
	"DeleteIdentityProvider":           "settings",
	"DeleteLogDestination":             "settings",
	"DeleteNamespaceConfigValue":       "config",
	"DeleteNotificationChannel":        "settings",
	"DeletePoolConfigValue":            "config",
	"DeletePoolRoleLink":               "config",
	"DeleteReportSchedule":             "schedules",
	"DeleteServiceApiKey":              "api-keys",
	"DeleteServiceNotificationChannel": "settings",
	"DeleteServiceTag":                 "settings",
	"DeleteStage":                      "catalog",
	"DeleteStorageClass":               "catalog",
	"DeleteSwarmTemplate":              "swarm-templates",
	"DeleteTeamExternalSystem":         "catalog",
	"DisableSwarmTemplate":             "swarm-templates",
	"EnableSwarmTemplate":              "swarm-templates",
	"PutAgentConfigValue":              "config",
	"PutAgentTemplate":                 "catalog",
	"PutBudget":                        "settings",
	"PutNamespaceConfigValue":          "config",
	"PutPoolConfigValue":               "config",
	"PutPoolExternalSystem":            "catalog",
	"PutServiceTag":                    "settings",
	"PutSetting":                       "settings",
	"PutStorageClass":                  "catalog",
	"RemoveMember":                     "members",
	"RemoveSecurityOperator":           "members",
	"SetAgentExternalSystemState":      "catalog",
	"SetTeamExternalSystemState":       "catalog",
	"UpdateCatalogSystem":              "catalog",
	"UpdateIdentityProvider":           "settings",
	"UpdateLogDestination":             "settings",
	"UpdateMember":                     "members",
	"UpdateModel":                      "catalog",
	"UpdateNotificationChannel":        "settings",
	"UpdateReportSchedule":             "schedules",
	"UpdateSecret":                     "config",
	"UpdateServiceNotificationChannel": "settings",
	"UpdateStage":                      "catalog",
	"UpdateSwarmTemplate":              "swarm-templates",
	"UpdateTeamExternalSystem":         "catalog",
	"VerifyAgentRoleLink":              "config",
	"VerifyPoolRoleLink":               "config",
}

// FR-R16 AC2: the matrix's NoOutbox set and the contract's exemption list are the same set.
func TestNoOutboxMatchesContractExemptions(t *testing.T) {
	regMu.Lock()
	got := map[string]bool{}
	for op, c := range cases {
		if c.NoOutbox != "" {
			got[op] = true
		}
	}
	regMu.Unlock()
	var extra, missing []string
	for op := range got {
		if _, ok := contractOutboxExempt[op]; !ok {
			extra = append(extra, op)
		}
	}
	for op := range contractOutboxExempt {
		if !got[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		t.Errorf("exempt in the matrix but not in the contract list: %v", extra)
	}
	if len(missing) > 0 {
		t.Errorf("in the contract list but not exempt in the matrix: %v", missing)
	}
}
