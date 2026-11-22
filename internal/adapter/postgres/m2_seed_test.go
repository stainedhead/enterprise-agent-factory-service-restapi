package postgres_test

import (
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

const testDigest = "sha256:4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f"

// seedModel makes sure a model exists and is enabled for the team; it returns the model id.
func seedModel(t *testing.T, db *pgtest.DB, teamID string) string {
	t.Helper()
	a := db.Admin(t)
	const id = "anthropic.claude-m2-test"
	if _, err := a.Exec(t.Context(), `INSERT INTO afs.models (id, provider, display_name, max_output_tokens) VALUES ($1, 'anthropic', 'M2 test', 4096) ON CONFLICT DO NOTHING`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(t.Context(), `INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, teamID, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedAgent inserts an agent with the given name through the admin connection.
func seedAgent(t *testing.T, db *pgtest.DB, teamID, name string) string {
	t.Helper()
	model := seedModel(t, db, teamID)
	id := pgtest.NewID("agt")
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.agents (id, team_id, name, template, image, model_id)
VALUES ($1, $2, $3, 'claude-code', 'registry.example.com/agents/coder@`+testDigest+`', $4)`, id, teamID, name, model); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return id
}

func arnFor(path string) string {
	return "arn:aws:secretsmanager:us-east-1:111122223333:secret:" + path + "-AbCdEf"
}

func lower(s string) string { return strings.ToLower(s) }
