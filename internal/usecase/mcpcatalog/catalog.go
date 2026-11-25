package mcpcatalog

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ServerInput is a create or update body. On update only the members sent change.
type ServerInput struct {
	Slug               *string
	DisplayName        *string
	Owner              *string
	Purpose            *string
	DocURL             *string
	DataClassification *string
	Transport          *string
	Location           *string
	URL                *string
	CABundle           *string
	Hosted             *domain.McpHosted
	AuthKind           *string
	AuthHeader         *string
	Federation         *domain.McpFederation
	Limits             *domain.McpLimits
	CredentialMode     *string
	DriftPolicy        *string
	DefaultForNewTeams *bool
	AllowSampling      *bool
	TrustAnnotations   *bool
}

func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

// apply copies the members sent onto x.
func (in ServerInput) apply(x *domain.McpServerRecord) {
	set(&x.Slug, in.Slug)
	set(&x.DisplayName, in.DisplayName)
	set(&x.Owner, in.Owner)
	set(&x.Purpose, in.Purpose)
	set(&x.DocURL, in.DocURL)
	set(&x.DataClassification, in.DataClassification)
	if in.Transport != nil {
		x.Transport = domain.McpTransport(*in.Transport)
		if x.Transport == domain.McpTransportHosted {
			x.URL = ""
		} else {
			x.Hosted = nil
		}
	}
	if in.Location != nil {
		x.Location = domain.EndpointLocation(*in.Location)
	}
	set(&x.URL, in.URL)
	set(&x.CABundle, in.CABundle)
	if in.Hosted != nil {
		h := *in.Hosted
		if h.InstanceMode == "" {
			h.InstanceMode = "per_team"
		}
		x.Hosted = &h
	}
	if in.AuthKind != nil {
		x.AuthKind = domain.McpAuthKind(*in.AuthKind)
		if x.AuthKind != domain.McpAuthWorkloadFederation {
			x.Federation = nil
		}
	}
	set(&x.AuthHeader, in.AuthHeader)
	if in.Federation != nil {
		f := *in.Federation
		x.Federation = &f
	}
	set(&x.Limits, in.Limits)
	set(&x.CredentialMode, in.CredentialMode)
	set(&x.DriftPolicy, in.DriftPolicy)
	set(&x.DefaultForNewTeams, in.DefaultForNewTeams)
	set(&x.AllowSampling, in.AllowSampling)
	set(&x.TrustAnnotations, in.TrustAnnotations)
}

// ServerView is a server with the fields the API computes.
type ServerView struct {
	Server          domain.McpServerRecord
	CredentialState string
	Approver        string
	Adoption        *Adoption
}

// validate checks a server against its own rules and the settings: host deny list and image registries.
func validate(st Settings, x domain.McpServerRecord) error {
	if err := x.Validate(); err != nil {
		return err
	}
	if err := x.Limits.Validate(); err != nil {
		return err
	}
	if x.CABundle != "" && (len(x.CABundle) > 16384 || !strings.HasPrefix(x.CABundle, "-----BEGIN CERTIFICATE-----")) {
		return domain.Validation(domain.FieldError{Field: "caBundle", Reason: "must be PEM certificates, at most 16384 characters"})
	}
	if !slices.Contains([]string{domain.McpCredentialShared, domain.McpCredentialPerTeam}, x.CredentialMode) {
		return domain.Validation(domain.FieldError{Field: "credentialMode", Reason: "must be shared or per_team"})
	}
	if !slices.Contains([]string{domain.McpDriftHold, domain.McpDriftAutoCompatible}, x.DriftPolicy) {
		return domain.Validation(domain.FieldError{Field: "driftPolicy", Reason: "must be hold or auto_approve_compatible"})
	}
	if h := x.Host(); h != "" && mcp.HostDenied(h, st.HostDenyList) {
		return domain.Validation(domain.FieldError{Field: "url", Reason: domain.ReasonHostDenied})
	}
	if x.Hosted != nil && !domain.RegistryAllowed(x.Hosted.Image, st.AllowedRegistries) {
		return domain.Validation(domain.FieldError{Field: "hosted.imageDigest", Reason: domain.ReasonRegistryNotAllowed})
	}
	return nil
}

// assess recomputes the risk tier from what is known now: location, host, auth, classification and inventory.
func assess(st Settings, x *domain.McpServerRecord, prims []domain.McpPrimitiveRecord) {
	ps := make([]mcp.Primitive, 0, len(prims))
	for _, p := range prims {
		ps = append(ps, toPrimitive(x.Slug, p))
	}
	r := mcp.Assess(mcp.RiskInput{Location: mcp.Location(x.Location), Host: x.Host(), HostDenyList: st.HostDenyList,
		AuthKind: mcp.CredentialKind(x.AuthKind), Classification: mcp.Classification(x.DataClassification), Primitives: ps,
		Findings: mcp.CheckDefinitions(ps, nil)})
	x.RiskTier, x.RiskReasons = string(r.Tier), r.Reasons
}

// toPrimitive turns a stored primitive into the policy package's primitive (annotations, description, params).
func toPrimitive(slug string, p domain.McpPrimitiveRecord) mcp.Primitive {
	out := mcp.Primitive{Server: slug, Kind: mcp.Kind(p.Kind), Name: p.Name, Title: p.Title, Status: mcp.Status(p.Status)}
	_ = json.Unmarshal(p.Annotations, &out.Annotations)
	def := p.Definition
	if len(p.PendingDefinition) > 0 {
		def = p.PendingDefinition
	}
	var d struct {
		Description string `json:"description"`
		InputSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"inputSchema"`
		Arguments []struct {
			Name string `json:"name"`
		} `json:"arguments"`
	}
	if json.Unmarshal(def, &d) == nil {
		out.Description = d.Description
		for k := range d.InputSchema.Properties {
			out.Params = append(out.Params, k)
		}
		for _, a := range d.Arguments {
			out.Params = append(out.Params, a.Name)
		}
		slices.Sort(out.Params)
	}
	return out
}

// view adds the computed fields.
func (s *Service) view(ctx context.Context, tx port.Tx, st Settings, x domain.McpServerRecord, adoption bool) (ServerView, error) {
	v := ServerView{Server: x}
	cred, err := s.Repo.GetCredential(ctx, tx, x.ID, x.CredentialScope(""))
	if err != nil {
		return v, err
	}
	if !x.IsTeam() && x.CredentialMode == domain.McpCredentialPerTeam && cred == nil && x.AuthKind.NeedsCredential() {
		v.CredentialState = domain.McpCredMissing
	} else {
		v.CredentialState = domain.McpCredentialStateOf(x.AuthKind, cred)
	}
	if x.ReviewStatus == domain.McpPendingReview {
		v.Approver = approverFor(st, x)
	}
	if adoption {
		a, err := s.Repo.Adoption(ctx, tx, x.ID)
		if err != nil {
			return v, err
		}
		v.Adoption = &a
	}
	return v, nil
}

// List implements ListMcpServers.
func (s *Service) List(ctx context.Context, c acting.Caller, f ServerFilter, p acting.Page) ([]ServerView, string, error) {
	if err := s.checkService(ctx, c, "ListMcpServers", authz.McpServersRead); err != nil {
		return nil, "", err
	}
	if f.Scope != "" && !slices.Contains([]string{"enterprise", "team", "all"}, f.Scope) {
		return nil, "", domain.Validation(domain.FieldError{Field: "scope", Reason: "must be enterprise, team or all"})
	}
	p = p.Normalized()
	var out []ServerView
	var next string
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := s.Repo.ListServers(ctx, tx, f, p.Cursor, p.Limit+1)
		if err != nil {
			return err
		}
		var page []domain.McpServerRecord
		page, next = acting.Slice(rows, p.Limit, func(x domain.McpServerRecord) string { return x.ID })
		out = make([]ServerView, 0, len(page))
		for _, x := range page {
			v, err := s.view(ctx, tx, st, x, false)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, next, err
}

// Get implements GetMcpServer: any server, with checks, risk and adoption.
func (s *Service) Get(ctx context.Context, c acting.Caller, id string) (ServerView, error) {
	if err := s.checkService(ctx, c, "GetMcpServer", authz.McpServersRead); err != nil {
		return ServerView{}, err
	}
	var out ServerView
	err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		x, err := s.Repo.GetServer(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out, err = s.view(ctx, tx, st, x, true)
		return err
	})
	return out, err
}

// Created is a server with the replayed response of an idempotent create.
type Created struct {
	View   ServerView
	Replay *port.StoredResponse
}

// create stores a new draft server for teamID ("" for the enterprise catalog).
func (s *Service) create(ctx context.Context, c acting.Caller, action, teamID string, in ServerInput, idem *acting.Idem,
	gate func(ctx context.Context, tx port.Tx) error) (Created, error) {
	var res Created
	x := domain.McpServerRecord{McpServer: domain.McpServer{TeamID: teamID, ReviewStatus: domain.McpDraft},
		CredentialMode: domain.McpCredentialPerTeam, DriftPolicy: domain.McpDriftHold}
	if teamID != "" {
		x.CredentialMode = domain.McpCredentialShared
	}
	in.apply(&x)
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		res = Created{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if gate != nil {
			if err := gate(ctx, tx); err != nil {
				return err
			}
		}
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		if err := validate(st, x); err != nil {
			return err
		}
		if x.ID, err = s.newID(domain.PrefixMcpServer); err != nil {
			return err
		}
		assess(st, &x, nil)
		if err := s.Repo.InsertServer(ctx, tx, x); err != nil {
			return err
		}
		stored, err := s.Repo.GetServer(ctx, tx, x.ID, false)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindServer, stored.ID, teamID, nil, auditServer(stored))); err != nil {
			return err
		}
		if err := s.emitServer(ctx, tx, c, domain.EventMcpServerCreated, stored, nil); err != nil {
			return err
		}
		if res.View, err = s.view(ctx, tx, st, stored, false); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.View)
	})
	return res, err
}

// Create implements CreateMcpServer: an enterprise server, created draft.
func (s *Service) Create(ctx context.Context, c acting.Caller, in ServerInput, idem *acting.Idem) (Created, error) {
	if err := s.checkService(ctx, c, "CreateMcpServer", authz.McpServersWrite); err != nil {
		return Created{}, err
	}
	if in.DefaultForNewTeams != nil && *in.DefaultForNewTeams {
		return Created{}, domain.Validation(domain.FieldError{Field: "defaultForNewTeams", Reason: domain.ReasonServerNotPublished})
	}
	return s.create(ctx, c, "CreateMcpServer", "", in, idem, nil)
}

// manifestNameRE takes the last path segment of a registry name (io.github.acme/issues → issues).
var manifestSlugRE = regexp.MustCompile(`[^a-z0-9-]+`)

// Manifest is the part of an MCP Registry server.json (preview format) a draft is pre-filled from.
type Manifest struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	WebsiteURL  string `json:"websiteUrl"`
	Remotes     []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"remotes"`
	Packages []struct {
		RegistryType string `json:"registryType"`
		Identifier   string `json:"identifier"`
		Version      string `json:"version"`
		FileSHA256   string `json:"fileSha256"`
	} `json:"packages"`
}

// FromManifest pre-fills a create body from a server.json; a slug sent overrides the derived one. It never counts
// as approval: the result is a draft like any other.
func FromManifest(raw json.RawMessage, slug string) (ServerInput, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil || m.Name == "" {
		return ServerInput{}, domain.Validation(domain.FieldError{Field: "manifest", Reason: "must be an MCP Registry server.json with a name"})
	}
	if slug == "" {
		last := m.Name[strings.LastIndexAny(m.Name, "/.")+1:]
		slug = strings.Trim(manifestSlugRE.ReplaceAllString(strings.ToLower(last), "-"), "-")
		if len(slug) > 31 {
			slug = strings.TrimRight(slug[:31], "-")
		}
	}
	title := m.Title
	if title == "" {
		title = m.Name
	}
	if len(title) > 100 {
		title = title[:100]
	}
	desc := m.Description
	if len(desc) > 1000 {
		desc = desc[:1000]
	}
	in := ServerInput{Slug: &slug, DisplayName: &title, Purpose: &desc, DataClassification: ptr("internal"), AuthKind: ptr("none")}
	if strings.HasPrefix(m.WebsiteURL, "https://") {
		in.DocURL = ptr(m.WebsiteURL)
	}
	for _, r := range m.Remotes {
		if r.Type == "streamable-http" || r.Type == "streamable_http" {
			in.Transport, in.Location, in.URL = ptr("streamable_http"), ptr("external"), ptr(r.URL)
			return in, nil
		}
	}
	for _, p := range m.Packages {
		if p.RegistryType == "oci" {
			img := p.Identifier
			if !strings.Contains(img, "@sha256:") && p.FileSHA256 != "" {
				img += "@sha256:" + p.FileSHA256
			}
			in.Transport, in.Location = ptr("hosted"), ptr("in_namespace")
			in.Hosted = &domain.McpHosted{Image: img, InstanceMode: "per_team"}
			return in, nil
		}
	}
	return ServerInput{}, domain.Validation(domain.FieldError{Field: "manifest", Reason: "needs a streamable-http remote or an oci package"})
}

func ptr[T any](v T) *T { return &v }

// Import implements ImportMcpServerManifest: a draft pre-filled from the manifest.
func (s *Service) Import(ctx context.Context, c acting.Caller, manifest json.RawMessage, slug string, idem *acting.Idem) (Created, error) {
	if err := s.checkService(ctx, c, "ImportMcpServerManifest", authz.McpServersWrite); err != nil {
		return Created{}, err
	}
	in, err := FromManifest(manifest, slug)
	if err != nil {
		return Created{}, err
	}
	return s.create(ctx, c, "ImportMcpServerManifest", "", in, idem, nil)
}

// update changes a server; a change of url, image, location, transport, auth or federation returns an approved
// server to pending_review (and unpublishes nothing: the publication waits on the new review).
func (s *Service) update(ctx context.Context, c acting.Caller, action, id string, in ServerInput, expect domain.Version,
	scope func(domain.McpServerRecord) error) (ServerView, error) {
	var out ServerView
	err := s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		cur, err := s.lockServer(ctx, tx, id, &expect)
		if err != nil {
			return err
		}
		if err := scope(cur); err != nil {
			return err
		}
		st, err := s.Repo.Settings(ctx, tx)
		if err != nil {
			return err
		}
		next := cur
		in.apply(&next)
		if err := validate(st, next); err != nil {
			return err
		}
		changing := domain.ReviewChangingUpdate(cur.McpServer, next.McpServer)
		if changing && cur.ReviewStatus == domain.McpApproved {
			next.ReviewStatus = domain.McpPendingReview
		}
		prims, err := s.Repo.ListPrimitives(ctx, tx, id, PrimitiveFilter{}, "", domain.MaxMcpPrimitivesPerServer)
		if err != nil {
			return err
		}
		assess(st, &next, prims)
		stored, err := s.store(ctx, tx, next)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindServer, id, cur.TeamID, auditServer(cur), auditServer(stored))); err != nil {
			return err
		}
		if err := s.emitServer(ctx, tx, c, domain.EventMcpServerUpdated, stored, map[string]any{"changes": changedFields(cur, stored)}); err != nil {
			return err
		}
		if changing && cur.ReviewStatus == domain.McpApproved {
			if err := s.emitServer(ctx, tx, c, domain.EventMcpServerSubmitted, stored, map[string]any{"riskTier": stored.RiskTier,
				"approver": approverFor(st, stored)}); err != nil {
				return err
			}
		}
		out, err = s.view(ctx, tx, st, stored, false)
		return err
	})
	return out, err
}

// changedFields names the audit fields whose value differs.
func changedFields(a, b domain.McpServerRecord) []string {
	x, y := auditServer(a), auditServer(b)
	var out []string
	for k, v := range y {
		if bv, _ := json.Marshal(v); string(bv) != mustJSON(x[k]) {
			out = append(out, k)
		}
	}
	for k := range x {
		if _, ok := y[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func enterpriseOnly(x domain.McpServerRecord) error {
	if x.IsTeam() {
		return domain.Validation(domain.FieldError{Field: "mcpServerId", Reason: "a team server changes through its team's path"})
	}
	return nil
}

// Update implements UpdateMcpServer (enterprise servers).
func (s *Service) Update(ctx context.Context, c acting.Caller, id string, in ServerInput, expect domain.Version) (ServerView, error) {
	if err := s.checkService(ctx, c, "UpdateMcpServer", authz.McpServersWrite); err != nil {
		return ServerView{}, err
	}
	if in.Slug != nil {
		return ServerView{}, domain.Validation(domain.FieldError{Field: "slug", Reason: "cannot change"})
	}
	return s.update(ctx, c, "UpdateMcpServer", id, in, expect, enterpriseOnly)
}

// remove deletes a server that no team has switched on.
func (s *Service) remove(ctx context.Context, c acting.Caller, action, id string, expect domain.Version, scope func(domain.McpServerRecord) error) error {
	return s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		cur, err := s.lockServer(ctx, tx, id, &expect)
		if err != nil {
			return err
		}
		if err := scope(cur); err != nil {
			return err
		}
		inUse, err := s.Repo.ServerInUse(ctx, tx, id)
		if err != nil {
			return err
		}
		if inUse {
			return domain.Conflict(domain.ReasonServerInUse, "switch the server off for every team first", nil)
		}
		if err := s.Repo.DeleteServer(ctx, tx, id, expect); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindServer, id, cur.TeamID, auditServer(cur), nil)); err != nil {
			return err
		}
		return s.emitServer(ctx, tx, c, domain.EventMcpServerDeleted, cur, nil)
	})
}

// Delete implements DeleteMcpServer: refused while any team has it on (409 server_in_use).
func (s *Service) Delete(ctx context.Context, c acting.Caller, id string, expect domain.Version) error {
	if err := s.checkService(ctx, c, "DeleteMcpServer", authz.McpServersWrite); err != nil {
		return err
	}
	return s.remove(ctx, c, "DeleteMcpServer", id, expect, enterpriseOnly)
}

// transition applies a change to a locked server and records it: audit, event, and the stored result.
type transition struct {
	action string
	scope  func(domain.McpServerRecord) error
	change func(st Settings, cur domain.McpServerRecord, next *domain.McpServerRecord) (domain.EventType, map[string]any, error)
	after  func(ctx context.Context, tx port.Tx, st Settings, stored domain.McpServerRecord) (domain.McpServerRecord, error)
}

func (s *Service) runIn(ctx context.Context, tx port.Tx, c acting.Caller, id string, expect *domain.Version, t transition) (ServerView, error) {
	cur, err := s.lockServer(ctx, tx, id, expect)
	if err != nil {
		return ServerView{}, err
	}
	if t.scope != nil {
		if err := t.scope(cur); err != nil {
			return ServerView{}, err
		}
	}
	st, err := s.Repo.Settings(ctx, tx)
	if err != nil {
		return ServerView{}, err
	}
	next := cur
	ev, data, err := t.change(st, cur, &next)
	if err != nil {
		return ServerView{}, err
	}
	stored := cur
	if ev != "" {
		if stored, err = s.store(ctx, tx, next); err != nil {
			return ServerView{}, err
		}
	}
	if t.after != nil {
		if stored, err = t.after(ctx, tx, st, stored); err != nil {
			return ServerView{}, err
		}
	}
	if ev != "" {
		if err := s.Audit.Write(ctx, tx, c.Record(t.action, kindServer, id, cur.TeamID, auditServer(cur), auditServer(stored))); err != nil {
			return ServerView{}, err
		}
		if err := s.emitServer(ctx, tx, c, ev, stored, data); err != nil {
			return ServerView{}, err
		}
	}
	return s.view(ctx, tx, st, stored, false)
}

// Publish implements PublishMcpServer: an approved enterprise server joins the catalog teams see.
func (s *Service) Publish(ctx context.Context, c acting.Caller, id string, idem *acting.Idem) (ServerView, *port.StoredResponse, error) {
	if err := s.checkService(ctx, c, "PublishMcpServer", authz.McpServersWrite); err != nil {
		return ServerView{}, nil, err
	}
	var replay *port.StoredResponse
	v, err := s.runIdem(ctx, c, id, nil, transition{action: "PublishMcpServer", scope: enterpriseOnly,
		change: func(_ Settings, cur domain.McpServerRecord, next *domain.McpServerRecord) (domain.EventType, map[string]any, error) {
			if cur.ReviewStatus != domain.McpApproved {
				return "", nil, domain.Conflict(domain.ReasonServerNotApproved, "only an approved server can be published", nil)
			}
			if cur.Published {
				return "", nil, nil
			}
			next.Published = true
			return domain.EventMcpServerPublished, nil, nil
		}}, idem, &replay)
	return v, replay, err
}

// Retire implements RetireMcpServer: the server leaves the catalog; teams that have it on keep it until they switch
// it off, and no team can switch it on again.
func (s *Service) Retire(ctx context.Context, c acting.Caller, id string, idem *acting.Idem) (ServerView, *port.StoredResponse, error) {
	if err := s.checkService(ctx, c, "RetireMcpServer", authz.McpServersWrite); err != nil {
		return ServerView{}, nil, err
	}
	var replay *port.StoredResponse
	v, err := s.runIdem(ctx, c, id, nil, transition{action: "RetireMcpServer", scope: enterpriseOnly,
		change: func(_ Settings, cur domain.McpServerRecord, next *domain.McpServerRecord) (domain.EventType, map[string]any, error) {
			if !cur.Published {
				return "", nil, nil
			}
			next.Published, next.DefaultForNewTeams = false, false
			return domain.EventMcpServerRetired, nil, nil
		}}, idem, &replay)
	return v, replay, err
}

// Suspend implements SuspendMcpServer: any server, with a reason; the gateway stops admitting it within 60 s.
func (s *Service) Suspend(ctx context.Context, c acting.Caller, id, reason string, idem *acting.Idem) (ServerView, *port.StoredResponse, error) {
	if err := s.checkService(ctx, c, "SuspendMcpServer", authz.McpServersSuspend); err != nil {
		return ServerView{}, nil, err
	}
	if err := domain.ValidateMcpReason("reason", reason); err != nil {
		return ServerView{}, nil, err
	}
	var replay *port.StoredResponse
	v, err := s.runIdem(ctx, c, id, nil, transition{action: "SuspendMcpServer",
		change: func(_ Settings, cur domain.McpServerRecord, next *domain.McpServerRecord) (domain.EventType, map[string]any, error) {
			if cur.ReviewStatus == domain.McpSuspended {
				return "", nil, domain.Conflict(domain.ReasonInvalidReviewTransition, "the server is already suspended", nil)
			}
			next.ReviewStatus, next.SuspendedFrom, next.SuspendedReason = domain.McpSuspended, cur.ReviewStatus, reason
			return domain.EventMcpServerSuspended, map[string]any{"reason": reason}, nil
		}}, idem, &replay)
	return v, replay, err
}

// Resume implements ResumeMcpServer: the server returns to the status it was suspended from.
func (s *Service) Resume(ctx context.Context, c acting.Caller, id, reason string, idem *acting.Idem) (ServerView, *port.StoredResponse, error) {
	if err := s.checkService(ctx, c, "ResumeMcpServer", authz.McpServersSuspend); err != nil {
		return ServerView{}, nil, err
	}
	if err := domain.ValidateMcpReason("reason", reason); err != nil {
		return ServerView{}, nil, err
	}
	var replay *port.StoredResponse
	v, err := s.runIdem(ctx, c, id, nil, transition{action: "ResumeMcpServer",
		change: func(_ Settings, cur domain.McpServerRecord, next *domain.McpServerRecord) (domain.EventType, map[string]any, error) {
			if cur.ReviewStatus != domain.McpSuspended {
				return "", nil, domain.Conflict(domain.ReasonInvalidReviewTransition, "the server is not suspended", nil)
			}
			next.ReviewStatus, next.SuspendedFrom, next.SuspendedReason = cur.SuspendedFrom, "", ""
			return domain.EventMcpServerResumed, map[string]any{"reason": reason}, nil
		}}, idem, &replay)
	return v, replay, err
}

// refresh asks the gateway to refresh a server's inventory within 2 minutes (§4.12: audited, no event).
func (s *Service) refresh(ctx context.Context, c acting.Caller, action, id string, scope func(domain.McpServerRecord) error) error {
	return s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.GetServer(ctx, tx, id, false)
		if err != nil {
			return err
		}
		if err := scope(cur); err != nil {
			return err
		}
		if err := s.Repo.RequestRefresh(ctx, tx, id); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record(action, kindServer, id, cur.TeamID, nil, map[string]any{"refreshRequested": true}))
	})
}

// Refresh implements RefreshMcpInventory (→ 202).
func (s *Service) Refresh(ctx context.Context, c acting.Caller, id string) error {
	if err := s.checkService(ctx, c, "RefreshMcpInventory", authz.McpServersWrite); err != nil {
		return err
	}
	return s.refresh(ctx, c, "RefreshMcpInventory", id, func(domain.McpServerRecord) error { return nil })
}
