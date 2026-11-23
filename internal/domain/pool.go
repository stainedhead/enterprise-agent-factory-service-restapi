package domain

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Swarm limits (database 000012; API contract §4.4a and §5).
const (
	// MaxPoolWorkers is the largest worker count of a pool; 0 keeps the pool defined with no pods.
	MaxPoolWorkers = 200
	// MinScratchGiB and MaxScratchGiB bound the per-worker scratch volume.
	MinScratchGiB = 1
	MaxScratchGiB = 500
	// DefaultScratchGiB is the scratch volume of a pool that names none (the database default).
	DefaultScratchGiB = 20
	// MinStagePosition and MaxStagePosition bound a stage's position (swarm_stages.position).
	MinStagePosition = 1
	MaxStagePosition = 50
	// MaxHandoffs bounds the pools one pool hands work to.
	MaxHandoffs = 50
	// MaxPoolPersona bounds the persona text.
	MaxPoolPersona = 10000
)

var poolRoleNameRE = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{1,64}$`)

// Stage is an ordered group of pools in a swarm (an ephemeral team). Position orders the stages.
type Stage struct {
	ID          string
	TeamID      string
	Name        Name
	Position    int
	Description string
	Version     Version
	CreatedAt   time.Time
	CreatedBy   string
	UpdatedAt   time.Time
}

// NewStageParams is a CreateStage body.
type NewStageParams struct {
	Name        string
	Position    int
	Description string
}

func validateStagePosition(p int) []FieldError {
	if p < MinStagePosition || p > MaxStagePosition {
		return []FieldError{{Field: "position", Reason: "must be " + itoa(MinStagePosition) + " to " + itoa(MaxStagePosition)}}
	}
	return nil
}

func validateStageDescription(d string) []FieldError {
	if utf8.RuneCountInString(d) > 500 {
		return []FieldError{{Field: "description", Reason: "must be at most 500 characters"}}
	}
	return nil
}

// NewStage validates p and returns the stage.
func NewStage(id, teamID string, p NewStageParams, now time.Time) (Stage, error) {
	var fe []FieldError
	name, err := ParseName("name", p.Name)
	fe = append(fe, fieldErrorsOf(err)...)
	fe = append(fe, validateStagePosition(p.Position)...)
	fe = append(fe, validateStageDescription(p.Description)...)
	if len(fe) > 0 {
		return Stage{}, Validation(fe...)
	}
	return Stage{ID: id, TeamID: teamID, Name: name, Position: p.Position, Description: p.Description,
		Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

// StagePatch is an UpdateStage body; a nil field is left unchanged. The name never changes.
type StagePatch struct {
	Position    *int
	Description *string
}

// Apply returns the stage with the patch applied.
func (s Stage) Apply(p StagePatch, now time.Time) (Stage, error) {
	var fe []FieldError
	if p.Position != nil {
		fe = append(fe, validateStagePosition(*p.Position)...)
		s.Position = *p.Position
	}
	if p.Description != nil {
		fe = append(fe, validateStageDescription(*p.Description)...)
		s.Description = *p.Description
	}
	if len(fe) > 0 {
		return s, Validation(fe...)
	}
	s.UpdatedAt = now.UTC()
	return s, nil
}

// PoolWorkers counts a pool's workers by the state in their latest activity window
// (afs.pool_workers_current).
type PoolWorkers struct {
	Desired   int
	Reporting int
	Working   int
	Idle      int
	Impaired  int
}

// Pool is a desired-state swarm pool: one identity shared by Workers pods (a Deployment). Pools exist
// only in ephemeral teams. Name and team never change: they name the pool's identity and secret paths.
type Pool struct {
	ID          string
	TeamID      string
	Name        Name
	Description string
	StageID     string
	Template    string
	Image       ImageRef
	Persona     string
	// ModelID is the deprecated alias of Models.DefaultModelID (kept until 2.0.0).
	ModelID string
	// Models is the pool's model access (MOD-3).
	Models         AgentModels
	Workers        int
	Resources      AgentResources
	ScratchSizeGiB int
	SharedVolume   string
	State          PoolState
	// Handoffs lists the pool ids this pool hands work to.
	Handoffs   []string
	Version    Version
	Generation Generation
	Status     Status
	CreatedAt  time.Time
	CreatedBy  string
	UpdatedAt  time.Time
}

// Paused reports whether the pool is paused.
func (p Pool) Paused() bool { return p.State == PoolStatePaused }

// NewPoolParams is a CreatePool body. Zero ScratchSizeGiB and empty SharedVolume take the defaults.
type NewPoolParams struct {
	Name        string
	Description string
	StageID     string
	Template    string
	Image       string
	Persona     string
	// ModelID is the deprecated alias of Models.DefaultModelID (CD-31).
	ModelID        string
	Models         *ModelsInput
	Workers        int
	Resources      AgentResources
	ScratchSizeGiB int
	SharedVolume   string
}

func validateWorkers(n int) []FieldError {
	if n < 0 || n > MaxPoolWorkers {
		return []FieldError{{Field: "workers", Reason: "must be 0 to " + itoa(MaxPoolWorkers)}}
	}
	return nil
}

func validateScratch(n int) []FieldError {
	if n < MinScratchGiB || n > MaxScratchGiB {
		return []FieldError{{Field: "scratchSizeGiB", Reason: "must be " + itoa(MinScratchGiB) + " to " + itoa(MaxScratchGiB)}}
	}
	return nil
}

func validateSharedVolume(v string) []FieldError {
	if !SharedVolume(v).Valid() {
		return []FieldError{{Field: "sharedVolume", Reason: "must be none or efs"}}
	}
	return nil
}

func validateStageID(id string) []FieldError {
	if id == "" {
		return nil
	}
	if _, err := ParseID(PrefixStage, id); err != nil {
		return []FieldError{{Field: "stageId", Reason: "must be a stg_ identifier"}}
	}
	return nil
}

func validatePoolText(description, persona string) []FieldError {
	var fe []FieldError
	if utf8.RuneCountInString(description) > 1000 {
		fe = append(fe, FieldError{Field: "description", Reason: "must be at most 1000 characters"})
	}
	if utf8.RuneCountInString(persona) > MaxPoolPersona {
		fe = append(fe, FieldError{Field: "persona", Reason: "must be at most 10000 characters"})
	}
	return fe
}

// NewPool validates p and returns the pool in state enabled, generation 1. That the team is
// ephemeral, the template, model, stage and name are free is checked by the use case.
func NewPool(id, teamID string, p NewPoolParams, now time.Time) (Pool, error) {
	var fe []FieldError
	name, err := ParseName("name", p.Name)
	fe = append(fe, fieldErrorsOf(err)...)
	image, err := ParseImageRef("image", p.Image)
	fe = append(fe, fieldErrorsOf(err)...)
	if !templateRE.MatchString(p.Template) {
		fe = append(fe, FieldError{Field: "template", Reason: "must be a template name (^[a-z][a-z0-9-]{1,62}$)"})
	}
	if p.Name == OrchestratorName {
		fe = append(fe, FieldError{Field: "name", Reason: ReasonNameReserved})
	}
	fe = append(fe, validatePoolText(p.Description, p.Persona)...)
	fe = append(fe, p.Resources.validate()...)
	fe = append(fe, validateWorkers(p.Workers)...)
	if p.ScratchSizeGiB == 0 {
		p.ScratchSizeGiB = DefaultScratchGiB
	}
	fe = append(fe, validateScratch(p.ScratchSizeGiB)...)
	if p.SharedVolume == "" {
		p.SharedVolume = string(SharedVolumeNone)
	}
	fe = append(fe, validateSharedVolume(p.SharedVolume)...)
	fe = append(fe, validateStageID(p.StageID)...)
	if len(fe) > 0 {
		return Pool{}, Validation(fe...)
	}
	return Pool{ID: id, TeamID: teamID, Name: name, Description: p.Description, StageID: p.StageID, Template: p.Template,
		Image: image, Persona: p.Persona, Workers: p.Workers, Resources: p.Resources,
		ScratchSizeGiB: p.ScratchSizeGiB, SharedVolume: p.SharedVolume, State: PoolStateEnabled, Handoffs: []string{},
		ModelID: strings.TrimSpace(p.ModelID), Models: AgentModels{Access: ModelAccessAll, DefaultModelID: strings.TrimSpace(p.ModelID)},
		Version: 1, Generation: 1, Status: PendingStatus(now), CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

// PoolPatch is an UpdatePool body: a nil field is left unchanged; StageID null clears the stage.
type PoolPatch struct {
	Description *string
	StageID     OptionalString
	Template    *string
	Image       *string
	Persona     *string
	// ModelID is the deprecated alias of Models.DefaultModelID; alone it sets only the default (CD-31).
	ModelID        *string
	Models         *ModelsInput
	Workers        *int
	Resources      *AgentResources
	ScratchSizeGiB *int
	SharedVolume   *string
}

// Apply returns p with the patch applied and the generation raised. A pool being deleted cannot change.
func (p Pool) Apply(patch PoolPatch, now time.Time) (Pool, error) {
	if p.State == PoolStateDeleting || p.State == PoolStateDeleted {
		return p, Conflict("invalid_state", "a pool in state "+string(p.State)+" cannot be changed", nil)
	}
	var fe []FieldError
	if patch.Description != nil {
		p.Description = *patch.Description
	}
	if patch.Persona != nil {
		p.Persona = *patch.Persona
	}
	fe = append(fe, validatePoolText(p.Description, p.Persona)...)
	if patch.StageID.Set {
		p.StageID = ""
		if patch.StageID.Value != nil {
			p.StageID = *patch.StageID.Value
		}
		fe = append(fe, validateStageID(p.StageID)...)
	}
	if patch.Template != nil {
		if !templateRE.MatchString(*patch.Template) {
			fe = append(fe, FieldError{Field: "template", Reason: "must be a template name"})
		}
		p.Template = *patch.Template
	}
	if patch.Image != nil {
		img, err := ParseImageRef("image", *patch.Image)
		fe = append(fe, fieldErrorsOf(err)...)
		p.Image = img
	}
	if patch.Workers != nil {
		fe = append(fe, validateWorkers(*patch.Workers)...)
		p.Workers = *patch.Workers
	}
	if patch.Resources != nil {
		fe = append(fe, patch.Resources.validate()...)
		p.Resources = *patch.Resources
	}
	if patch.ScratchSizeGiB != nil {
		fe = append(fe, validateScratch(*patch.ScratchSizeGiB)...)
		p.ScratchSizeGiB = *patch.ScratchSizeGiB
	}
	if patch.SharedVolume != nil {
		fe = append(fe, validateSharedVolume(*patch.SharedVolume)...)
		p.SharedVolume = *patch.SharedVolume
	}
	if len(fe) > 0 {
		return p, Validation(fe...)
	}
	p.Generation++
	p.UpdatedAt = now.UTC()
	return p, nil
}

func (p Pool) transition(to PoolState, now time.Time) Pool {
	p.State = to
	p.Generation++
	p.UpdatedAt = now.UTC()
	return p
}

// Scale returns the pool with n workers (0 to 200). changed is false when n is the current count.
func (p Pool) Scale(n int, now time.Time) (out Pool, changed bool, err error) {
	if fe := validateWorkers(n); len(fe) > 0 {
		return p, false, Validation(fe...)
	}
	if p.State == PoolStateDeleting || p.State == PoolStateDeleted {
		return p, false, Conflict("invalid_state", "a pool in state "+string(p.State)+" cannot be scaled", nil)
	}
	if n == p.Workers {
		return p, false, nil
	}
	p.Workers = n
	p.Generation++
	p.UpdatedAt = now.UTC()
	return p, true, nil
}

// Pause returns the pool paused: the Deployment scales to zero and the identity stays. Pausing a
// paused pool is a no-op.
func (p Pool) Pause(now time.Time) (out Pool, changed bool, err error) {
	switch p.State {
	case PoolStatePaused:
		return p, false, nil
	case PoolStateEnabled:
		return p.transition(PoolStatePaused, now), true, nil
	}
	return p, false, Conflict("invalid_state", "a pool in state "+string(p.State)+" cannot be paused", nil)
}

// Resume returns the pool enabled again with its desired worker count; resuming a running pool is a no-op.
func (p Pool) Resume(now time.Time) (out Pool, changed bool, err error) {
	switch p.State {
	case PoolStateEnabled:
		return p, false, nil
	case PoolStatePaused:
		return p.transition(PoolStateEnabled, now), true, nil
	}
	return p, false, Conflict("invalid_state", "a pool in state "+string(p.State)+" cannot be resumed", nil)
}

// MarkDeleting returns the pool in state deleting with status Deleting; the reconciler runs the delete
// steps and finalizes it. Deleting a deleting pool is a no-op.
func (p Pool) MarkDeleting(now time.Time) (out Pool, changed bool, err error) {
	switch p.State {
	case PoolStateDeleting:
		return p.WithDeletingStatus(), false, nil
	case PoolStateDeleted:
		return p, false, NotFound("pool")
	}
	n := p.transition(PoolStateDeleting, now)
	return n.WithDeletingStatus(), true, nil
}

// WithDeletingStatus shows phase Deleting while the pool is deleting, because the reconciler may not
// have written the status yet.
func (p Pool) WithDeletingStatus() Pool {
	if p.State == PoolStateDeleting && p.Status.Phase != PhaseDeleting {
		p.Status.Phase = PhaseDeleting
	}
	return p
}

// ValidateHandoffs checks the pools fromID hands work to: pool ids, not the pool itself, no repeats.
func ValidateHandoffs(fromID string, to []string) error {
	var fe []FieldError
	if len(to) > MaxHandoffs {
		fe = append(fe, FieldError{Field: "toPoolIds", Reason: "at most " + itoa(MaxHandoffs) + " pools"})
	}
	seen := map[string]bool{}
	for i, id := range to {
		f := "toPoolIds[" + itoa(i) + "]"
		switch {
		case func() bool { _, err := ParseID(PrefixPool, id); return err != nil }():
			fe = append(fe, FieldError{Field: f, Reason: "must be a pool_ identifier"})
		case id == fromID:
			fe = append(fe, FieldError{Field: f, Reason: "a pool cannot hand work to itself"})
		case seen[id]:
			fe = append(fe, FieldError{Field: f, Reason: id + " is listed twice"})
		}
		seen[id] = true
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// PoolConfigValue is one entry of a pool's configuration: its own literal value or a link to the
// namespace value of the same name. ResolvedValue is the effective value.
type PoolConfigValue struct {
	ID            string
	TeamID        string
	PoolID        string
	Name          ConfigName
	Source        ConfigSource
	Value         *string
	ResolvedValue *string
	Version       Version
	UpdatedAt     time.Time
}

// PoolRoleLink is the pool's one shared named role in an organization account (AGT-8): the same role
// name in every account, one profile per account. Its role and external id never change; replace the
// link instead. The reconciler writes Status, LastVerifiedAt and LastError.
type PoolRoleLink struct {
	ID                string
	TeamID            string
	PoolID            string
	AccountID         string
	RoleName          string
	RoleARN           string
	ProfileName       string
	ExternalID        string
	MaxSessionSeconds int
	Status            RoleLinkStatus
	LastVerifiedAt    *time.Time
	LastError         string
	Version           Version
	CreatedAt         time.Time
	CreatedBy         string
	UpdatedAt         time.Time
}

// NewPoolRoleLinkParams is the request of CreatePoolRoleLink.
type NewPoolRoleLinkParams struct {
	RoleName          string
	AccountID         string
	ProfileName       string
	MaxSessionSeconds int
}

// Validate checks the request shape. The account filter and the pool's one role name are checked
// against the database by the use case.
func (p NewPoolRoleLinkParams) Validate() error {
	var fe []FieldError
	switch {
	case p.RoleName == "":
		fe = append(fe, FieldError{Field: "roleName", Reason: "is required"})
	case !poolRoleNameRE.MatchString(p.RoleName):
		fe = append(fe, FieldError{Field: "roleName", Reason: "must be an IAM role name (1 to 64 characters of letters, digits and +=,.@_-)"})
	}
	switch {
	case p.AccountID == "":
		fe = append(fe, FieldError{Field: "accountId", Reason: "is required"})
	case !ValidAWSAccountID(p.AccountID):
		fe = append(fe, FieldError{Field: "accountId", Reason: "must be a 12 digit AWS account id"})
	}
	switch {
	case p.ProfileName == "":
		fe = append(fe, FieldError{Field: "profileName", Reason: "is required"})
	case !profileNameRE.MatchString(p.ProfileName):
		fe = append(fe, FieldError{Field: "profileName", Reason: "must start with a lowercase letter and hold lowercase letters, digits and hyphens, at most 63 characters"})
	}
	if p.MaxSessionSeconds != 0 && (p.MaxSessionSeconds < MinRoleLinkSessionSeconds || p.MaxSessionSeconds > MaxRoleLinkSessionSeconds) {
		fe = append(fe, FieldError{Field: "maxSessionSeconds", Reason: "must be between 900 and 43200"})
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// NewPoolRoleLink builds a pending_trust link for a pool. The caller gives the id and the external id.
func NewPoolRoleLink(id, teamID, poolID, externalID string, p NewPoolRoleLinkParams, now time.Time) (PoolRoleLink, error) {
	if err := p.Validate(); err != nil {
		return PoolRoleLink{}, err
	}
	max := p.MaxSessionSeconds
	if max == 0 {
		max = DefaultRoleLinkSessionSeconds
	}
	return PoolRoleLink{ID: id, TeamID: teamID, PoolID: poolID, AccountID: p.AccountID, RoleName: p.RoleName,
		RoleARN: "arn:aws:iam::" + p.AccountID + ":role/" + p.RoleName, ProfileName: p.ProfileName, ExternalID: externalID,
		MaxSessionSeconds: max, Status: RoleLinkStatusPendingTrust, Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

// Audit is the audit view of the link: nothing secret is in it, the external id is a guard.
func (l PoolRoleLink) Audit() map[string]any {
	return map[string]any{"id": l.ID, "poolId": l.PoolID, "accountId": l.AccountID, "roleName": l.RoleName,
		"profileName": l.ProfileName, "maxSessionSeconds": l.MaxSessionSeconds, "status": string(l.Status)}
}
