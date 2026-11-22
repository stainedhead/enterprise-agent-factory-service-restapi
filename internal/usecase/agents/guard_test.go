package agents_test

import (
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
)

// TestGuardRefusals calls the shared validator directly (FR-R01 AC5): one row per refused case, each
// naming the field the API reports, plus the allowed case.
func TestGuardRefusals(t *testing.T) {
	const otherDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	r := newRig()
	r.repo.Templates["retired"] = agents.TemplateInfo{ID: "tpl_2", Name: "retired", Enabled: false, AllowedDigests: []string{digest}}
	g := agents.Guard{Src: r.repo}
	ctx := t.Context()

	for _, tc := range []struct {
		name  string
		check func() error
		field string // empty: allowed
	}{
		{"template unknown", func() error { _, err := g.CheckTemplate(ctx, nil, "nope", image); return err }, "template"},
		{"template disabled", func() error { _, err := g.CheckTemplate(ctx, nil, "retired", image); return err }, "template"},
		{"digest not allowed", func() error {
			_, err := g.CheckTemplate(ctx, nil, "claude-code", "registry.example.com/agents/coder@"+otherDigest)
			return err
		}, "image"},
		{"template and digest ok", func() error { _, err := g.CheckTemplate(ctx, nil, "claude-code", image); return err }, ""},
		{"model not enabled for the team", func() error { return g.CheckModel(ctx, nil, team, "other.model") }, "modelId"},
		{"model enabled", func() error { return g.CheckModel(ctx, nil, team, model) }, ""},
		{"storage class not offered", func() error { return g.CheckStorageClass(ctx, nil, "io9") }, "volume.storageClass"},
		{"storage class offered", func() error { return g.CheckStorageClass(ctx, nil, "gp3") }, ""},
		{"binding of an unknown secret", func() error {
			return g.CheckBindings(ctx, nil, team, "agt_1", []domain.SecretBinding{{SecretID: "sec_missing"}})
		}, "secretBindings[0].secretId"},
		{"binding of another agent's secret", func() error {
			return g.CheckBindings(ctx, nil, team, "agt_1", []domain.SecretBinding{{SecretID: secB}})
		}, "secretBindings[0].secretId"},
		{"binding of a pool secret", func() error {
			return g.CheckBindings(ctx, nil, team, "agt_1", []domain.SecretBinding{{SecretID: secC}})
		}, "secretBindings[0].secretId"},
		{"binding of a team secret", func() error {
			return g.CheckBindings(ctx, nil, team, "agt_1", []domain.SecretBinding{{SecretID: secA}})
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.check()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			var de *domain.Error
			if !errors.As(err, &de) || domain.KindOf(err) != domain.KindValidation || !hasField(de, tc.field) {
				t.Fatalf("err = %v, want a validation error on %s", err, tc.field)
			}
		})
	}
}

func TestGuardPassesSourceErrorsThrough(t *testing.T) {
	r := newRig()
	boom := errors.New("db down")
	r.repo.Err = map[string]error{"Template": boom, "ModelEnabled": boom, "StorageClassEnabled": boom, "Secrets": boom}
	g := agents.Guard{Src: r.repo}
	if _, err := g.CheckTemplate(t.Context(), nil, "claude-code", image); !errors.Is(err, boom) {
		t.Errorf("template: %v", err)
	}
	if err := g.CheckModel(t.Context(), nil, team, model); !errors.Is(err, boom) {
		t.Errorf("model: %v", err)
	}
	if err := g.CheckStorageClass(t.Context(), nil, "gp3"); !errors.Is(err, boom) {
		t.Errorf("class: %v", err)
	}
	if err := g.CheckBindings(t.Context(), nil, team, "a", []domain.SecretBinding{{SecretID: secA}}); !errors.Is(err, boom) {
		t.Errorf("bindings: %v", err)
	}
}
