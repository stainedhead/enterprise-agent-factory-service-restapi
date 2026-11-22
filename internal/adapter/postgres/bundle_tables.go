package postgres

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Columns of each table that travel in an export bundle (root CFG-5). The lists are allowlists shared by
// the exporter and the importer: nothing else is read into a bundle or written from one, so a bundle can
// never set a version, a creator or a status, and a column holding a secret value cannot be added by
// accident. No column here can hold a secret value, an API key hash or a client secret.
var (
	bundleTeamCols = []string{"slug", "display_name", "description", "cluster_id", "cost_center", "owner_email", "max_budget_usd",
		"quotas", "labels", "allowed_aws_accounts", "may_add_external_systems", "may_manage_aws_accounts", "isolation_tier",
		"viewer_kubectl"}
	bundleTagCols       = []string{"key", "value"}
	bundleBudgetCols    = []string{"period", "amount_usd", "thresholds", "on_exhaust", "starts_at", "expires_at"}
	bundleNsConfigCols  = []string{"name", "config_value", "description"}
	bundleExtSystemCols = []string{"slug", "name", "category", "description", "documentation_url", "owner", "purpose", "endpoints", "enabled"}
	bundleAgentCols     = []string{"name", "description", "template", "image", "persona", "model_id", "resources", "state", "storage_size_gib",
		"storage_class", "storage_on_delete", "snapshot_schedule", "snapshot_retain_count", "iam_identity"}
	bundleAgentConfigCols = []string{"name", "source", "config_value"}
	bundleRoleLinkCols    = []string{"account_id", "role_arn", "profile_name", "external_id", "max_session_seconds"}
)

// obj builds a jsonb_build_object expression over the columns of alias.
func obj(alias string, cols []string, extra ...string) string {
	parts := make([]string, 0, len(cols)+len(extra))
	for _, c := range cols {
		parts = append(parts, "'"+c+"', "+alias+"."+c)
	}
	parts = append(parts, extra...)
	return "jsonb_build_object(" + strings.Join(parts, ", ") + ")"
}

// decodeRow reads one jsonb value into a Row keeping numbers exactly as PostgreSQL wrote them.
func decodeRow(raw []byte) (domain.Row, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var r domain.Row
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	return r, nil
}
