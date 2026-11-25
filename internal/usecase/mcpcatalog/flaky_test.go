package mcpcatalog_test

import (
	"context"
	"errors"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// errBoom is what a flaky repository returns.
var errBoom = errors.New("boom")

// flaky fails its failAt-th call (1-based) with errBoom and passes every other call to the fake.
type flaky struct {
	*repo
	failAt, calls int
}

func (f *flaky) tick() error {
	f.calls++
	if f.calls == f.failAt {
		return errBoom
	}
	return nil
}

func (fl *flaky) Settings(ctx context.Context, tx port.Tx) (mcpcatalog.Settings, error) {
	if err := fl.tick(); err != nil {
		return mcpcatalog.Settings{}, err
	}
	return fl.repo.Settings(ctx, tx)
}

func (fl *flaky) ListServers(ctx context.Context, tx port.Tx, f mcpcatalog.ServerFilter, after string, limit int) ([]domain.McpServerRecord, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.ListServers(ctx, tx, f, after, limit)
}

func (fl *flaky) GetServer(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.McpServerRecord, error) {
	if err := fl.tick(); err != nil {
		return domain.McpServerRecord{}, err
	}
	return fl.repo.GetServer(ctx, tx, id, forUpdate)
}

func (fl *flaky) InsertServer(ctx context.Context, tx port.Tx, s domain.McpServerRecord) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.InsertServer(ctx, tx, s)
}

func (fl *flaky) UpdateServer(ctx context.Context, tx port.Tx, s domain.McpServerRecord, expect domain.Version) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.UpdateServer(ctx, tx, s, expect)
}

func (fl *flaky) DeleteServer(ctx context.Context, tx port.Tx, id string, expect domain.Version) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.DeleteServer(ctx, tx, id, expect)
}

func (fl *flaky) ServerInUse(ctx context.Context, tx port.Tx, id string) (bool, error) {
	if err := fl.tick(); err != nil {
		return false, err
	}
	return fl.repo.ServerInUse(ctx, tx, id)
}

func (fl *flaky) Adoption(ctx context.Context, tx port.Tx, id string) (mcpcatalog.Adoption, error) {
	if err := fl.tick(); err != nil {
		return mcpcatalog.Adoption{}, err
	}
	return fl.repo.Adoption(ctx, tx, id)
}

func (fl *flaky) RequestRefresh(ctx context.Context, tx port.Tx, id string) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.RequestRefresh(ctx, tx, id)
}

func (fl *flaky) ListPrimitives(ctx context.Context, tx port.Tx, serverID string, f mcpcatalog.PrimitiveFilter, after string, limit int) ([]domain.McpPrimitiveRecord, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.ListPrimitives(ctx, tx, serverID, f, after, limit)
}

func (fl *flaky) GetPrimitive(ctx context.Context, tx port.Tx, serverID, id string, forUpdate bool) (domain.McpPrimitiveRecord, error) {
	if err := fl.tick(); err != nil {
		return domain.McpPrimitiveRecord{}, err
	}
	return fl.repo.GetPrimitive(ctx, tx, serverID, id, forUpdate)
}

func (fl *flaky) DecidePrimitive(ctx context.Context, tx port.Tx, id, status string) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.DecidePrimitive(ctx, tx, id, status)
}

func (fl *flaky) InsertReview(ctx context.Context, tx port.Tx, r domain.McpReview) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.InsertReview(ctx, tx, r)
}

func (fl *flaky) ReviewQueue(ctx context.Context, tx port.Tx) ([]domain.McpReviewQueueItem, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.ReviewQueue(ctx, tx)
}

func (fl *flaky) ListRules(ctx context.Context, tx port.Tx, s mcpcatalog.PolicyScope) ([]domain.McpPolicyRule, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.ListRules(ctx, tx, s)
}

func (fl *flaky) ReplaceRules(ctx context.Context, tx port.Tx, s mcpcatalog.PolicyScope, rules []domain.McpPolicyRule, ids []string) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.ReplaceRules(ctx, tx, s, rules, ids)
}

func (fl *flaky) TeamInfo(ctx context.Context, tx port.Tx, teamID string) (mcpcatalog.TeamInfo, error) {
	if err := fl.tick(); err != nil {
		return mcpcatalog.TeamInfo{}, err
	}
	return fl.repo.TeamInfo(ctx, tx, teamID)
}

func (fl *flaky) ListTeamServers(ctx context.Context, tx port.Tx, teamID string) ([]domain.TeamMcpServerRow, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.ListTeamServers(ctx, tx, teamID)
}

func (fl *flaky) GetTeamServer(ctx context.Context, tx port.Tx, teamID, serverID string) (domain.TeamMcpServerRow, error) {
	if err := fl.tick(); err != nil {
		return domain.TeamMcpServerRow{}, err
	}
	return fl.repo.GetTeamServer(ctx, tx, teamID, serverID)
}

func (fl *flaky) PutSwitch(ctx context.Context, tx port.Tx, sw domain.TeamMcpSwitch) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.PutSwitch(ctx, tx, sw)
}

func (fl *flaky) TeamEnabledServers(ctx context.Context, tx port.Tx, teamID string) ([]string, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.TeamEnabledServers(ctx, tx, teamID)
}

func (fl *flaky) GetCredential(ctx context.Context, tx port.Tx, serverID, scope string) (*domain.McpCredentialRecord, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.GetCredential(ctx, tx, serverID, scope)
}

func (fl *flaky) CredentialPath(ctx context.Context, tx port.Tx, teamID, slug string) (string, error) {
	if err := fl.tick(); err != nil {
		return "", err
	}
	return fl.repo.CredentialPath(ctx, tx, teamID, slug)
}

func (fl *flaky) UpsertCredential(ctx context.Context, tx port.Tx, c domain.McpCredentialRecord) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.UpsertCredential(ctx, tx, c)
}

func (fl *flaky) DeleteCredential(ctx context.Context, tx port.Tx, serverID, scope string) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.DeleteCredential(ctx, tx, serverID, scope)
}

func (fl *flaky) RequestFederationTest(ctx context.Context, tx port.Tx, serverID, teamID string) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.RequestFederationTest(ctx, tx, serverID, teamID)
}

func (fl *flaky) Subject(ctx context.Context, tx port.Tx, teamID, kind, id string) (domain.McpSubject, error) {
	if err := fl.tick(); err != nil {
		return domain.McpSubject{}, err
	}
	return fl.repo.Subject(ctx, tx, teamID, kind, id)
}

func (fl *flaky) SubjectAccount(ctx context.Context, tx port.Tx, teamID, kind, id string) (string, string, error) {
	if err := fl.tick(); err != nil {
		return "", "", err
	}
	return fl.repo.SubjectAccount(ctx, tx, teamID, kind, id)
}

func (fl *flaky) Admission(ctx context.Context, tx port.Tx, kind, id string) ([]domain.McpAdmissionRow, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.Admission(ctx, tx, kind, id)
}

func (fl *flaky) ListEvents(ctx context.Context, tx port.Tx, f domain.McpEventFilter, service bool, cursor string, limit int) ([]domain.McpEventRecord, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.ListEvents(ctx, tx, f, service, cursor, limit)
}

func (fl *flaky) UsageReport(ctx context.Context, tx port.Tx, q domain.McpUsageQuery) ([]domain.McpUsageRow, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.UsageReport(ctx, tx, q)
}

func (fl *flaky) OpenCapture(ctx context.Context, tx port.Tx) (*domain.McpQueryCapture, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.OpenCapture(ctx, tx)
}

func (fl *flaky) LatestCapture(ctx context.Context, tx port.Tx) (*domain.McpQueryCapture, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.LatestCapture(ctx, tx)
}

func (fl *flaky) InsertCapture(ctx context.Context, tx port.Tx, c domain.McpQueryCapture) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.InsertCapture(ctx, tx, c)
}

func (fl *flaky) StopCapture(ctx context.Context, tx port.Tx, id string, at time.Time, by string) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.StopCapture(ctx, tx, id, at, by)
}

func (fl *flaky) ListFederated(ctx context.Context, tx port.Tx, kind, teamID, subjectID string) ([]domain.FederatedIdentityRecord, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.ListFederated(ctx, tx, kind, teamID, subjectID)
}

func (fl *flaky) GetFederated(ctx context.Context, tx port.Tx, kind, teamID, subjectID, name string) (*domain.FederatedIdentityRecord, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.GetFederated(ctx, tx, kind, teamID, subjectID, name)
}

func (fl *flaky) InsertFederated(ctx context.Context, tx port.Tx, kind string, f domain.FederatedIdentityRecord) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.InsertFederated(ctx, tx, kind, f)
}

func (fl *flaky) UpdateFederated(ctx context.Context, tx port.Tx, kind string, f domain.FederatedIdentityRecord, expect domain.Version) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.UpdateFederated(ctx, tx, kind, f, expect)
}

func (fl *flaky) DeleteFederated(ctx context.Context, tx port.Tx, kind, teamID, subjectID, name string, expect domain.Version) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.DeleteFederated(ctx, tx, kind, teamID, subjectID, name, expect)
}

func (fl *flaky) InsertOAuthState(ctx context.Context, tx port.Tx, hash []byte, teamID, serverID string, expiresAt time.Time) error {
	if err := fl.tick(); err != nil {
		return err
	}
	return fl.repo.InsertOAuthState(ctx, tx, hash, teamID, serverID, expiresAt)
}

func (fl *flaky) ClaimOAuthState(ctx context.Context, tx port.Tx, hash []byte) (*mcpcatalog.OAuthState, error) {
	if err := fl.tick(); err != nil {
		return nil, err
	}
	return fl.repo.ClaimOAuthState(ctx, tx, hash)
}
