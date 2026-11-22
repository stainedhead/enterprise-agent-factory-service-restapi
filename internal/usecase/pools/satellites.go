package pools

import (
	"context"
	"slices"
	"strconv"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// writePool runs a write on a pool's satellite: the team must be ephemeral, the pool live, and fn runs
// in one transaction with the pool row locked.
func (s *Service) writePool(ctx context.Context, c acting.Caller, teamID, poolID string, fn func(ctx context.Context, tx port.Tx, p domain.Pool) error) error {
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireEphemeral(ctx, tx, teamID); err != nil {
			return err
		}
		p, err := s.Repo.Get(ctx, tx, teamID, poolID, true)
		if err != nil {
			return err
		}
		if p.State == domain.PoolStateDeleting || p.State == domain.PoolStateDeleted {
			return domain.Conflict("invalid_state", "a pool in state "+string(p.State)+" cannot be changed", nil)
		}
		return fn(ctx, tx, p)
	})
}

// readPool runs a read on a pool's satellite after proving the pool is visible.
func (s *Service) readPool(ctx context.Context, c acting.Caller, teamID, poolID string, fn func(ctx context.Context, tx port.Tx) error) error {
	return s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if _, err := s.Repo.Get(ctx, tx, teamID, poolID, false); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

// touched raises the generation of the pools whose rendering a satellite change affects and returns the
// owner's new version.
func (s *Service) touched(ctx context.Context, tx port.Tx, teamID, ownerID string, others ...string) (domain.Version, error) {
	ids := slices.Compact(slices.Sorted(slices.Values(append([]string{ownerID}, others...))))
	if err := s.Repo.Touch(ctx, tx, teamID, ids); err != nil {
		return 0, err
	}
	p, err := s.Repo.Get(ctx, tx, teamID, ownerID, false)
	return p.Version, err
}

// Handoffs is a pool's handoff set with the version to send as If-Match (the pool's).
type Handoffs struct {
	ToPoolIDs []string
	Version   domain.Version
}

// ListHandoffs implements ListPoolHandoffs.
func (s *Service) ListHandoffs(ctx context.Context, c acting.Caller, teamID, poolID string) (Handoffs, error) {
	var out Handoffs
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		p, err := s.get(ctx, tx, teamID, poolID, false)
		out = Handoffs{ToPoolIDs: p.Handoffs, Version: p.Version}
		return err
	})
	return out, err
}

// PutHandoffs implements PutPoolHandoffs: the pools poolID hands work to, replaced as a set. The pool
// and every pool whose inbox senders change are rendered again. expect is nil without If-Match (PUT is
// an upsert).
func (s *Service) PutHandoffs(ctx context.Context, c acting.Caller, teamID, poolID string, expect *domain.Version, to []string) (Handoffs, error) {
	if to == nil {
		to = []string{}
	}
	var out Handoffs
	err := s.writePool(ctx, c, teamID, poolID, func(ctx context.Context, tx port.Tx, p domain.Pool) error {
		if expect != nil {
			if err := domain.CheckVersion(p.Version, *expect); err != nil {
				return err
			}
		}
		if err := domain.ValidateHandoffs(poolID, to); err != nil {
			return err
		}
		if n, err := s.Repo.PoolsExist(ctx, tx, teamID, to); err != nil {
			return err
		} else if n != len(to) {
			return domain.Validation(domain.FieldError{Field: "toPoolIds", Reason: "every pool must be a live pool of this team"})
		}
		old, err := s.Repo.Handoffs(ctx, tx, teamID, poolID)
		if err != nil {
			return err
		}
		if err := s.Repo.ReplaceHandoffs(ctx, tx, teamID, poolID, to); err != nil {
			return err
		}
		v, err := s.touched(ctx, tx, teamID, poolID, append(slices.Clone(old), to...)...)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("PutPoolHandoffs", kindHandoff, poolID, teamID,
			map[string]any{"toPoolIds": old}, map[string]any{"toPoolIds": to})); err != nil {
			return err
		}
		p.Version = v
		if err := s.emit(ctx, tx, c, domain.EventPoolUpdated, p, map[string]any{"fields": []string{"handoffs"}}); err != nil {
			return err
		}
		out = Handoffs{ToPoolIDs: slices.Sorted(slices.Values(to)), Version: v}
		return nil
	})
	return out, err
}

// BindingSet is a pool's secret bindings with the pool's version.
type BindingSet struct {
	Items   []domain.SecretBinding
	Version domain.Version
}

// ListBindings implements ListPoolSecretBindings.
func (s *Service) ListBindings(ctx context.Context, c acting.Caller, teamID, poolID string) (BindingSet, error) {
	var out BindingSet
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		p, err := s.Repo.Get(ctx, tx, teamID, poolID, false)
		if err != nil {
			return err
		}
		if out.Items, err = s.Repo.Bindings(ctx, tx, teamID, poolID); err != nil {
			return err
		}
		out.Version = p.Version
		return nil
	})
	return out, err
}

// checkBindings refuses bindings of unknown secrets and of secrets owned by an agent or another pool.
func (s *Service) checkBindings(ctx context.Context, tx port.Tx, teamID, poolID string, bs []domain.SecretBinding) error {
	if len(bs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(bs))
	for _, b := range bs {
		ids = append(ids, b.SecretID)
	}
	found, err := s.Repo.Secrets(ctx, tx, teamID, ids)
	if err != nil {
		return err
	}
	var fe []domain.FieldError
	for i, b := range bs {
		f := "items[" + strconv.Itoa(i) + "].secretId"
		info, ok := found[b.SecretID]
		switch {
		case !ok:
			fe = append(fe, domain.FieldError{Field: f, Reason: "no such secret in the team"})
		case info.AgentID != "" || (info.PoolID != "" && info.PoolID != poolID):
			fe = append(fe, domain.FieldError{Field: f, Reason: "the secret belongs to an agent or another pool and cannot be bound here"})
		}
	}
	if len(fe) > 0 {
		return domain.Validation(fe...)
	}
	return nil
}

// PutBindings implements PutPoolSecretBindings: the pool's bindings, replaced as a set. The secrets
// are the team's shared ones or this pool's own (scope pool).
func (s *Service) PutBindings(ctx context.Context, c acting.Caller, teamID, poolID string, expect *domain.Version, bs []domain.SecretBinding) (BindingSet, error) {
	if bs == nil {
		bs = []domain.SecretBinding{}
	}
	var out BindingSet
	err := s.writePool(ctx, c, teamID, poolID, func(ctx context.Context, tx port.Tx, p domain.Pool) error {
		if expect != nil {
			if err := domain.CheckVersion(p.Version, *expect); err != nil {
				return err
			}
		}
		if err := domain.ValidateBindings(bs); err != nil {
			return renameField(err, "secretBindings", "items")
		}
		if err := s.checkBindings(ctx, tx, teamID, poolID, bs); err != nil {
			return err
		}
		old, err := s.Repo.Bindings(ctx, tx, teamID, poolID)
		if err != nil {
			return err
		}
		if err := s.Repo.ReplaceBindings(ctx, tx, teamID, poolID, bs); err != nil {
			return err
		}
		v, err := s.touched(ctx, tx, teamID, poolID)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("PutPoolSecretBindings", kindBinding, poolID, teamID,
			bindingAudit(old), bindingAudit(bs))); err != nil {
			return err
		}
		p.Version = v
		if err := s.emit(ctx, tx, c, domain.EventPoolUpdated, p, map[string]any{"fields": []string{"secretBindings"}}); err != nil {
			return err
		}
		out = BindingSet{Items: bs, Version: v}
		return nil
	})
	return out, err
}

func bindingAudit(bs []domain.SecretBinding) []map[string]string {
	out := make([]map[string]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, map[string]string{"secretId": b.SecretID, "delivery": b.Delivery, "target": b.Target})
	}
	return out
}

// renameField rewrites the field prefix of a validation error's fields.
func renameField(err error, from, to string) error {
	de, ok := err.(*domain.Error) //nolint:errorlint // domain.ValidateBindings returns the unwrapped error
	if !ok {
		return err
	}
	fs := slices.Clone(de.Fields)
	for i := range fs {
		if len(fs[i].Field) >= len(from) && fs[i].Field[:len(from)] == from {
			fs[i].Field = to + fs[i].Field[len(from):]
		}
	}
	return domain.Validation(fs...)
}

// ---- configuration values ----

func auditConfig(v domain.PoolConfigValue) map[string]any {
	m := map[string]any{"poolId": v.PoolID, "name": v.Name, "source": v.Source}
	if v.Value != nil {
		m["value"] = *v.Value
	}
	return m
}

// ListConfig implements ListPoolConfigValues: the pool's entries, resolved, with their source.
func (s *Service) ListConfig(ctx context.Context, c acting.Caller, teamID, poolID string, p acting.Page) ([]domain.PoolConfigValue, string, error) {
	p = p.Normalized()
	var rows []domain.PoolConfigValue
	err := s.readPool(ctx, c, teamID, poolID, func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListConfig(ctx, tx, teamID, poolID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(v domain.PoolConfigValue) string { return string(v.Name) })
	return page, next, nil
}

// ConfigInput is the body of PutPoolConfigValue: a literal needs a value, a namespace link must not have one.
type ConfigInput struct {
	Source domain.ConfigSource
	Value  *string
}

func (in ConfigInput) validate(name domain.ConfigName) error {
	switch in.Source {
	case domain.ConfigSourceLiteral:
		if in.Value == nil {
			return domain.Validation(domain.FieldError{Field: "value", Reason: "is required when source is literal"})
		}
		return domain.ValidateConfigValue("value", name, *in.Value)
	case domain.ConfigSourceNamespace:
		if in.Value != nil {
			return domain.Validation(domain.FieldError{Field: "value", Reason: "must be absent when source is namespace"})
		}
		return nil
	}
	return domain.Validation(domain.FieldError{Field: "source", Reason: "must be literal or namespace"})
}

// PutConfig implements PutPoolConfigValue: creates or updates the pool's entry. A link needs the
// namespace value of the same name. A change to an existing entry may carry If-Match. Contract §7 has no
// configuration event, so the write is the row and an audit record (CD-7); the reconciler restarts the
// workers from the change feed.
func (s *Service) PutConfig(ctx context.Context, c acting.Caller, teamID, poolID string, name domain.ConfigName, in ConfigInput, expect *domain.Version) (domain.PoolConfigValue, error) {
	if _, err := domain.ParseConfigName("name", string(name)); err != nil {
		return domain.PoolConfigValue{}, err
	}
	if err := in.validate(name); err != nil {
		return domain.PoolConfigValue{}, err
	}
	var out domain.PoolConfigValue
	err := s.writePool(ctx, c, teamID, poolID, func(ctx context.Context, tx port.Tx, _ domain.Pool) error {
		if in.Source == domain.ConfigSourceNamespace {
			ok, err := s.Repo.NamespaceValueExists(ctx, tx, teamID, name)
			if err != nil {
				return err
			}
			if !ok {
				return domain.Validation(domain.FieldError{Field: "source", Reason: "there is no namespace value named " + string(name) + " to link to"})
			}
		}
		next := domain.PoolConfigValue{TeamID: teamID, PoolID: poolID, Name: name, Source: in.Source, Value: in.Value}
		cur, err := s.Repo.GetConfig(ctx, tx, teamID, poolID, name, true)
		switch {
		case domain.KindOf(err) == domain.KindNotFound:
			if expect != nil {
				return domain.PreconditionFailed()
			}
			id, err := s.IDs.NewID(domain.PrefixPoolConfig)
			if err != nil {
				return err
			}
			next.ID = id.String()
			if err := s.Repo.InsertConfig(ctx, tx, next); err != nil {
				return err
			}
			if out, err = s.Repo.GetConfig(ctx, tx, teamID, poolID, name, false); err != nil {
				return err
			}
			return s.Audit.Write(ctx, tx, c.Record("PutPoolConfigValue", kindConfig, out.ID, teamID, nil, auditConfig(out)))
		case err != nil:
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		next.ID = cur.ID
		if err := s.Repo.UpdateConfig(ctx, tx, next, cur.Version); err != nil {
			return err
		}
		if out, err = s.Repo.GetConfig(ctx, tx, teamID, poolID, name, false); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("PutPoolConfigValue", kindConfig, out.ID, teamID, auditConfig(cur), auditConfig(out)))
	})
	return out, err
}

// DeleteConfig implements DeletePoolConfigValue (If-Match required).
func (s *Service) DeleteConfig(ctx context.Context, c acting.Caller, teamID, poolID string, name domain.ConfigName, expect domain.Version) error {
	return s.writePool(ctx, c, teamID, poolID, func(ctx context.Context, tx port.Tx, _ domain.Pool) error {
		cur, err := s.Repo.GetConfig(ctx, tx, teamID, poolID, name, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.DeleteConfig(ctx, tx, teamID, poolID, name, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeletePoolConfigValue", kindConfig, cur.ID, teamID, auditConfig(cur), nil))
	})
}

// ---- external systems ----

// ListSystems implements ListPoolExternalSystems: the effective systems of the pool, with disabledAt.
func (s *Service) ListSystems(ctx context.Context, c acting.Caller, teamID, poolID string, p acting.Page) ([]domain.ExternalSystemAccess, string, error) {
	p = p.Normalized()
	var rows []domain.ExternalSystemAccess
	err := s.readPool(ctx, c, teamID, poolID, func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListSystems(ctx, tx, teamID, poolID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(a domain.ExternalSystemAccess) string { return a.ID })
	return page, next, nil
}

func systemAudit(poolID string, a domain.ExternalSystemAccess) map[string]any {
	return map[string]any{"poolId": poolID, "systemId": a.ID, "slug": a.Slug, "enabled": a.On()}
}

// SetSystemState implements PutPoolExternalSystem. A pool cannot switch on a system its team or the
// service has off (409). If-Match, when sent, is compared with the system's version.
func (s *Service) SetSystemState(ctx context.Context, c acting.Caller, teamID, poolID, systemID string, enabled bool, expect *domain.Version) (domain.ExternalSystemAccess, error) {
	var out domain.ExternalSystemAccess
	err := s.writePool(ctx, c, teamID, poolID, func(ctx context.Context, tx port.Tx, _ domain.Pool) error {
		cur, err := s.Repo.GetSystem(ctx, tx, teamID, poolID, systemID)
		if err != nil {
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		if enabled && !cur.TeamOn() {
			return domain.Conflict("disabled_above", "the system is switched off at the "+string(cur.DisabledAt)+" level; a pool cannot switch it on",
				map[string]any{"disabledAt": string(cur.DisabledAt)})
		}
		oid, err := s.IDs.NewID(domain.PrefixExtSystemOptout)
		if err != nil {
			return err
		}
		if err := s.Repo.SetSystemOff(ctx, tx, teamID, poolID, systemID, oid.String(), !enabled); err != nil {
			return err
		}
		if out, err = s.Repo.GetSystem(ctx, tx, teamID, poolID, systemID); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("PutPoolExternalSystem", kindOptOut, systemID, teamID, systemAudit(poolID, cur), systemAudit(poolID, out)))
	})
	return out, err
}

// ---- role links ----

// LinkResult is a link with the replayed response of an idempotent call.
type LinkResult struct {
	Link   domain.PoolRoleLink
	Replay *port.StoredResponse
}

// ListLinks implements ListPoolRoleLinks.
func (s *Service) ListLinks(ctx context.Context, c acting.Caller, teamID, poolID string, p acting.Page) ([]domain.PoolRoleLink, string, error) {
	p = p.Normalized()
	var rows []domain.PoolRoleLink
	err := s.readPool(ctx, c, teamID, poolID, func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListLinks(ctx, tx, teamID, poolID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(l domain.PoolRoleLink) string { return l.ID })
	return page, next, nil
}

// CreateLink implements CreatePoolRoleLink (RA-US-10.4): the account must be active and inside the
// team's allowed accounts and the role name must be the one the pool's other links use (400 otherwise).
// The link starts pending_trust and the reconciler verifies it. Contract §7 has no event for it (CD-7).
func (s *Service) CreateLink(ctx context.Context, c acting.Caller, teamID, poolID string, in domain.NewPoolRoleLinkParams, idem *acting.Idem) (LinkResult, error) {
	var res LinkResult
	if err := in.Validate(); err != nil {
		return res, err
	}
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = LinkResult{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if err := s.requireEphemeral(ctx, tx, teamID); err != nil {
			return err
		}
		p, err := s.Repo.Get(ctx, tx, teamID, poolID, true)
		if err != nil {
			return err
		}
		if p.State == domain.PoolStateDeleting || p.State == domain.PoolStateDeleted {
			return domain.Conflict("invalid_state", "a pool in state "+string(p.State)+" cannot be changed", nil)
		}
		if ok, err := s.Repo.AccountAllowed(ctx, tx, teamID, in.AccountID); err != nil {
			return err
		} else if !ok {
			return domain.Validation(domain.FieldError{Field: "accountId", Reason: "account " + in.AccountID + " is not an active account in this team's allowed accounts"})
		}
		if role, err := s.Repo.LinkRoleName(ctx, tx, teamID, poolID); err != nil {
			return err
		} else if role != "" && role != in.RoleName {
			return domain.Validation(domain.FieldError{Field: "roleName", Reason: "a pool uses one shared role name in every account; this pool's is " + role})
		}
		id, err := s.IDs.NewID(domain.PrefixPoolRoleLink)
		if err != nil {
			return err
		}
		ext, err := domain.NewExternalID(s.Entropy)
		if err != nil {
			return err
		}
		l, err := domain.NewPoolRoleLink(id.String(), teamID, poolID, ext, in, s.Now())
		if err != nil {
			return err
		}
		if err := s.Repo.InsertLink(ctx, tx, l); err != nil {
			return err
		}
		if res.Link, err = s.Repo.GetLink(ctx, tx, teamID, poolID, l.ID, false); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreatePoolRoleLink", kindLink, l.ID, teamID, nil, res.Link.Audit())); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Link)
	})
	return res, err
}

// DeleteLink implements DeletePoolRoleLink (If-Match required).
func (s *Service) DeleteLink(ctx context.Context, c acting.Caller, teamID, poolID, linkID string, expect domain.Version) error {
	return s.writePool(ctx, c, teamID, poolID, func(ctx context.Context, tx port.Tx, _ domain.Pool) error {
		cur, err := s.Repo.GetLink(ctx, tx, teamID, poolID, linkID, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.DeleteLink(ctx, tx, teamID, poolID, linkID, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeletePoolRoleLink", kindLink, linkID, teamID, cur.Audit(), nil))
	})
}

// VerifyLink implements VerifyPoolRoleLink: the link goes back to pending_trust so the reconciler
// verifies it on its next pass. A disabled link is a Conflict: widen the allowed accounts first.
func (s *Service) VerifyLink(ctx context.Context, c acting.Caller, teamID, poolID, linkID string, idem *acting.Idem) (LinkResult, error) {
	var res LinkResult
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = LinkResult{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if err := s.requireEphemeral(ctx, tx, teamID); err != nil {
			return err
		}
		if _, err := s.Repo.Get(ctx, tx, teamID, poolID, false); err != nil {
			return err
		}
		cur, err := s.Repo.GetLink(ctx, tx, teamID, poolID, linkID, true)
		if err != nil {
			return err
		}
		if cur.Status == domain.RoleLinkStatusDisabled {
			return domain.Conflict("link_disabled", "the link is disabled because its account is outside the team's allowed accounts", nil)
		}
		l, err := s.Repo.RequestVerify(ctx, tx, teamID, poolID, linkID)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("VerifyPoolRoleLink", kindLink, linkID, teamID, cur.Audit(), l.Audit())); err != nil {
			return err
		}
		res.Link = l
		return idem.Complete(ctx, s.Idem, tx, l)
	})
	return res, err
}
