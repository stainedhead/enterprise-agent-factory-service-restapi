package teams_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

func reasonOf(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		if de.Reason != "" {
			return de.Reason
		}
		if len(de.Fields) > 0 {
			return de.Fields[0].Reason
		}
	}
	return ""
}

// ORC-2: CreateTeam creates the orchestration agent with the team, with the service's default tool, and
// its boot secrets; the limit defaults to the service setting.
func TestCreateTeamCreatesOrchestrationAgent(t *testing.T) {
	r := newRig(t)
	res, err := r.svc.Create(t.Context(), svcAdmin, createInput(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.repo.Orchestrations) != 1 {
		t.Fatalf("orchestration agents = %d", len(r.repo.Orchestrations))
	}
	o := r.repo.Orchestrations[0]
	if o.TeamID != res.Team.ID || o.Name != domain.OrchestratorName || o.Tool != domain.ToolPaperclip || o.Models.DefaultModelID != "m1" {
		t.Errorf("%+v", o)
	}
	if res.Team.OrchestrationAgentID != o.AgentID || r.boot.Tools[o.AgentID] != domain.ToolPaperclip {
		t.Errorf("team agent %q, boot %v", res.Team.OrchestrationAgentID, r.boot.Tools)
	}
}

func TestCreateTeamBuzzHasNoBootSecrets(t *testing.T) {
	r := newRig(t)
	in := createInput()
	in.Orchestration = &teams.OrchestrationInput{Tool: domain.ToolBuzz, WebUIExposure: domain.ExposureOff}
	if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.boot.Tools) != 0 || r.repo.Orchestrations[0].Tool != domain.ToolBuzz {
		t.Errorf("buzz: boot %v, agents %+v", r.boot.Tools, r.repo.Orchestrations)
	}
}

func TestCreateTeamOrchestrationRefusals(t *testing.T) {
	disable := func(r *rig, body string) { r.settings.Set(domain.SettingOrchestrationTools, json.RawMessage(body)) }
	tests := []struct {
		name  string
		setup func(*rig)
		mod   func(*teams.CreateTeamInput)
		want  domain.ErrorKind
		why   string
	}{
		{"tool not enabled", func(r *rig) {
			disable(r, `{"paperclip":{"enabled":true,"default":true,"templateId":"t","imageRepository":"i"},"buzz":{"enabled":false,"default":false,"templateId":null,"imageRepository":null}}`)
		}, func(in *teams.CreateTeamInput) { in.Orchestration = &teams.OrchestrationInput{Tool: domain.ToolBuzz} }, domain.KindValidation, domain.ReasonToolNotEnabled},
		{"runtime unset", func(r *rig) {
			disable(r, `{"paperclip":{"enabled":true,"default":true,"templateId":null,"imageRepository":null},"buzz":{"enabled":false,"default":false}}`)
		}, func(*teams.CreateTeamInput) {}, domain.KindConflict, domain.ReasonOrchestrationRuntimeUnset},
		{"unknown tool", nil, func(in *teams.CreateTeamInput) { in.Orchestration = &teams.OrchestrationInput{Tool: "slack"} }, domain.KindValidation, ""},
		{"buzz surface", nil, func(in *teams.CreateTeamInput) {
			in.Params.MaxCollaborationExposure = ptrExposure(domain.ExposurePublic)
			in.Orchestration = &teams.OrchestrationInput{Tool: domain.ToolBuzz, WebUIExposure: domain.ExposurePrivate}
		}, domain.KindValidation, domain.ReasonSurfaceNotSupported},
		{"above limit", nil, func(in *teams.CreateTeamInput) {
			in.Params.MaxCollaborationExposure = ptrExposure(domain.ExposurePrivate)
			in.Orchestration = &teams.OrchestrationInput{WebUIExposure: domain.ExposurePublic}
		}, domain.KindValidation, domain.ReasonExposureAboveLimit},
		{"manual for buzz", nil, func(in *teams.CreateTeamInput) {
			in.Orchestration = &teams.OrchestrationInput{Tool: domain.ToolBuzz, RegistrationMode: domain.RegistrationManual}
		}, domain.KindValidation, domain.ReasonRegistrationNotSupported},
		{"bad volume", nil, func(in *teams.CreateTeamInput) { in.Orchestration = &teams.OrchestrationInput{VolumeSizeGiB: 5} }, domain.KindValidation, ""},
		{"no model available", func(r *rig) { r.repo.Catalog["m1"] = teams.ModelInfo{ID: "m1", Enabled: true} }, func(*teams.CreateTeamInput) {}, domain.KindValidation, domain.ReasonNoModelAvailable},
		{"default not allowed", nil, func(in *teams.CreateTeamInput) {
			in.Orchestration = &teams.OrchestrationInput{Models: &domain.ModelsInput{DefaultModelID: ptrString("zzz")}}
		}, domain.KindValidation, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if tc.setup != nil {
				tc.setup(r)
			}
			in := createInput()
			tc.mod(&in)
			_, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
			if domain.KindOf(err) != tc.want || (tc.why != "" && reasonOf(err) != tc.why) {
				t.Fatalf("err = %v (reason %q)", err, reasonOf(err))
			}
			if len(r.repo.Orchestrations) != 0 || len(r.repo.Teams) != 0 {
				t.Error("a refused request left rows behind")
			}
		})
	}
}

// A failure creating the agent or its boot secrets rolls the whole team back and deletes written secrets.
func TestCreateTeamOrchestrationFailures(t *testing.T) {
	r := newRig(t)
	r.repo.OrchErr = domain.Conflict(domain.ReasonOrchestrationRuntimeUnset, "unset", nil)
	if _, err := r.svc.Create(t.Context(), svcAdmin, createInput(), nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("db refusal: %v", err)
	}
	r = newRig(t)
	r.boot.Err = domain.Unavailable("Secrets Manager", errors.New("down"))
	if _, err := r.svc.Create(t.Context(), svcAdmin, createInput(), nil); domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("secrets manager: %v", err)
	}
	if len(r.repo.Teams) != 0 {
		t.Error("team left behind")
	}
	r = newRig(t)
	r.svc.Boot = nil
	if _, err := r.svc.Create(t.Context(), svcAdmin, createInput(), nil); domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("no writer: %v", err)
	}
}

func ptrExposure(e domain.Exposure) *domain.Exposure { return &e }
func ptrString(s string) *string                     { return &s }

// Settings the use case reads are validated where they are read: a malformed value is an internal error, and
// the team model defaults follow models.default_team_access and models.default_team_models (MOD-2).
func TestCreateTeamReadsSettings(t *testing.T) {
	t.Run("malformed orchestration.tools", func(t *testing.T) {
		r := newRig(t)
		r.settings.Set(domain.SettingOrchestrationTools, json.RawMessage(`[1]`))
		if _, err := r.svc.Create(t.Context(), svcAdmin, createInput(), nil); domain.KindOf(err) != domain.KindInternal {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("malformed models.default_team_access", func(t *testing.T) {
		r := newRig(t)
		r.settings.Set(domain.SettingModelsDefaultTeamAccess, json.RawMessage(`5`))
		in := createInput()
		in.Models = nil
		if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); domain.KindOf(err) != domain.KindInternal {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown default access falls back to all", func(t *testing.T) {
		r := newRig(t)
		r.settings.Set(domain.SettingModelsDefaultTeamAccess, json.RawMessage(`"weird"`))
		in := createInput()
		in.Models = nil
		res, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
		if err != nil || res.Team.ID == "" {
			t.Fatalf("create: %v", err)
		}
	})
	t.Run("selected by default takes models.default_team_models", func(t *testing.T) {
		r := newRig(t)
		r.settings.Set(domain.SettingModelsDefaultTeamAccess, json.RawMessage(`"selected"`))
		r.settings.Set(domain.SettingModelsDefaultTeamModels, json.RawMessage(`["m1"]`))
		in := createInput()
		in.Models = nil
		res, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
		if err != nil || len(r.repo.TeamModels[res.Team.ID]) != 1 {
			t.Fatalf("create: %v models %v", err, r.repo.TeamModels)
		}
	})
	t.Run("malformed models.default_team_models", func(t *testing.T) {
		r := newRig(t)
		r.settings.Set(domain.SettingModelsDefaultTeamAccess, json.RawMessage(`"selected"`))
		r.settings.Set(domain.SettingModelsDefaultTeamModels, json.RawMessage(`"x"`))
		in := createInput()
		in.Models = nil
		if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); domain.KindOf(err) != domain.KindInternal {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("all with a list is refused", func(t *testing.T) {
		r := newRig(t)
		in := createInput()
		in.Params.ModelAccess = domain.ModelAccessAll
		if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("bad access value", func(t *testing.T) {
		r := newRig(t)
		in := createInput()
		in.Params.ModelAccess = "x"
		if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("err = %v", err)
		}
	})
}

func TestCreateEphemeralTeamReadsItsMaximum(t *testing.T) {
	r := newRig(t)
	r.settings.Set(domain.SettingTeamsEphemeralMaxDays, json.RawMessage(`"x"`))
	in := ephemeralInput(24*time.Hour, domain.TeardownRuleAtCapOrExpiry, "100")
	if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); err == nil {
		t.Error("a malformed maximum is accepted")
	}
}
