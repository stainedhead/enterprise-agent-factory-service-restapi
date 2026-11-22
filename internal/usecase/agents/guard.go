package agents

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// GuardSource is what the guardrail checks read: the catalog and the team's enabled models and
// secrets. Repository implements it.
type GuardSource interface {
	// Template returns the catalog template of that name.
	Template(ctx context.Context, tx port.Tx, name string) (TemplateInfo, bool, error)
	// ModelEnabled reports whether the model is enabled and offered to the team.
	ModelEnabled(ctx context.Context, tx port.Tx, teamID, modelID string) (bool, error)
	// StorageClassEnabled reports whether the storage class exists and is offered.
	StorageClassEnabled(ctx context.Context, tx port.Tx, name string) (bool, error)
	// Secrets returns the owners of the team's secrets with these ids; unknown ids are absent.
	Secrets(ctx context.Context, tx port.Tx, teamID string, ids []string) (map[string]SecretInfo, error)
}

var _ GuardSource = Repository(nil)

// Guard is the one place the agent guardrails live: the agents use case (CreateAgent, UpdateAgent) and
// the restore use case (RestoreRevision) both call it, so a service admin's restriction holds on every
// path that can change an agent. Each check returns a validation error naming the API field.
type Guard struct{ Src GuardSource }

// CheckTemplate refuses a template that is unknown, disabled or does not list the image's digest
// (RA-US-4.1, RA-US-4.3). It returns the template's id.
func (g Guard) CheckTemplate(ctx context.Context, tx port.Tx, name string, image domain.ImageRef, role domain.AgentRole) (string, error) {
	t, ok, err := g.Src.Template(ctx, tx, name)
	if err != nil {
		return "", err
	}
	switch {
	case !ok:
		return "", domain.Validation(domain.FieldError{Field: "template", Reason: "no agent template named " + name})
	case !t.Enabled:
		return "", domain.Validation(domain.FieldError{Field: "template", Reason: "agent template " + name + " is disabled"})
	}
	// A collaboration agent needs a template with the paperclip capability and a worker may not use one.
	has := slices.Contains(t.Capabilities, domain.CapabilityPaperclip)
	switch {
	case role == domain.AgentRoleCollaboration && !has:
		return "", domain.Validation(domain.FieldError{Field: "template", Reason: "agent template " + name + " does not have the paperclip capability"})
	case role != domain.AgentRoleCollaboration && has:
		return "", domain.Validation(domain.FieldError{Field: "template", Reason: "agent template " + name + " is a collaboration template; a worker cannot use it"})
	}
	digest := string(image)[strings.LastIndex(string(image), "@")+1:]
	if !slices.Contains(t.AllowedDigests, digest) {
		return "", domain.Validation(domain.FieldError{Field: "image", Reason: "digest " + digest + " is not allowed by agent template " + name})
	}
	return t.ID, nil
}

// CheckModel refuses a model that is not enabled for the team.
func (g Guard) CheckModel(ctx context.Context, tx port.Tx, teamID, model string) error {
	ok, err := g.Src.ModelEnabled(ctx, tx, teamID, model)
	if err != nil {
		return err
	}
	if !ok {
		return domain.Validation(domain.FieldError{Field: "modelId", Reason: model + " is not enabled for the team"})
	}
	return nil
}

// CheckStorageClass refuses a storage class that is not offered.
func (g Guard) CheckStorageClass(ctx context.Context, tx port.Tx, class string) error {
	ok, err := g.Src.StorageClassEnabled(ctx, tx, class)
	if err != nil {
		return err
	}
	if !ok {
		return domain.Validation(domain.FieldError{Field: "volume.storageClass", Reason: class + " is not an offered storage class"})
	}
	return nil
}

// CheckBindings refuses bindings of unknown secrets and of secrets owned by another agent or a
// pool (RA-US-4.1). agentID is the agent the bindings are for.
func (g Guard) CheckBindings(ctx context.Context, tx port.Tx, teamID, agentID string, bs []domain.SecretBinding) error {
	if len(bs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(bs))
	for _, b := range bs {
		ids = append(ids, b.SecretID)
	}
	found, err := g.Src.Secrets(ctx, tx, teamID, ids)
	if err != nil {
		return err
	}
	var fe []domain.FieldError
	for i, b := range bs {
		f := "secretBindings[" + strconv.Itoa(i) + "].secretId"
		info, ok := found[b.SecretID]
		switch {
		case !ok:
			fe = append(fe, domain.FieldError{Field: f, Reason: "no such secret in the team"})
		case info.ManagedBy == string(domain.ManagedByPaperclip):
			fe = append(fe, domain.FieldError{Field: f, Reason: "a secret Paperclip manages cannot be bound"})
		case info.PoolID != "" || (info.AgentID != "" && info.AgentID != agentID):
			fe = append(fe, domain.FieldError{Field: f, Reason: "the secret belongs to another agent or a pool and cannot be bound here"})
		}
	}
	if len(fe) > 0 {
		return domain.Validation(fe...)
	}
	return nil
}
