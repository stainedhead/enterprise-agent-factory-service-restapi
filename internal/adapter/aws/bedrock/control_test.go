package bedrock

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/awstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

const (
	srcARN  = "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-sonnet-4-5-v1:0"
	profARN = "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/abc123"
	pname   = "afs-01JD0000000000000000000001-anthropic-claude-sonnet-4-5-v1-0"
)

func summary(name, arn string) map[string]any {
	return map[string]any{
		"inferenceProfileName": name, "inferenceProfileArn": arn, "inferenceProfileId": "abc123", "type": "APPLICATION",
		"status": "ACTIVE", "createdAt": "2026-11-21T00:00:00Z", "updatedAt": "2026-11-21T00:00:00Z", "models": []any{map[string]any{"modelArn": srcARN}},
	}
}

func listHandler(pages ...map[string]any) awstest.Handler {
	i := 0
	return func(r awstest.Request) *awstest.Response {
		if r.Method != "GET" || r.Path != "/inference-profiles" {
			return nil
		}
		if i >= len(pages) {
			i = len(pages) - 1
		}
		resp := awstest.JSONOK(pages[i])
		i++
		return &resp
	}
}

func newControl(t *testing.T) (*Control, *awstest.Server) {
	t.Helper()
	srv := awstest.New(t)
	return NewControl(srv.Config()), srv
}

func spec() team.ProfileSpec {
	return team.ProfileSpec{Name: pname, SourceARN: srcARN, Tags: map[string]string{team.TagTeamID: "team_01JD0000000000000000000001", team.TagTeamSlug: "payments"}}
}

// M1-G2c: a missing profile is created from the model, tagged for attribution.
func TestEnsureProfileCreates(t *testing.T) {
	c, srv := newControl(t)
	srv.On(listHandler(map[string]any{"inferenceProfileSummaries": []any{summary("someone-elses", "arn:x")}}))
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Method == "POST" && r.Path == "/inference-profiles" {
			resp := awstest.JSONOK(map[string]any{"inferenceProfileArn": profARN, "status": "ACTIVE"})
			return &resp
		}
		return nil
	})
	p, err := c.EnsureProfile(t.Context(), spec())
	if err != nil || p.ARN != profARN || p.Name != pname {
		t.Fatalf("EnsureProfile = %+v, %v", p, err)
	}
	var body struct {
		InferenceProfileName string `json:"inferenceProfileName"`
		ModelSource          struct {
			CopyFrom string `json:"copyFrom"`
		} `json:"modelSource"`
		Tags               []struct{ Key, Value string } `json:"tags"`
		ClientRequestToken string                        `json:"clientRequestToken"`
	}
	for _, r := range srv.Requests() {
		if r.Method == "POST" {
			if err := r.JSON(&body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if body.InferenceProfileName != pname || body.ModelSource.CopyFrom != srcARN || body.ClientRequestToken == "" {
		t.Errorf("create body = %+v", body)
	}
	tags := map[string]string{}
	for _, tg := range body.Tags {
		tags[tg.Key] = tg.Value
	}
	if tags[team.TagTeamID] != "team_01JD0000000000000000000001" || tags[team.TagTeamSlug] != "payments" || tags[team.TagManagedBy] != team.ManagedByValue {
		t.Errorf("tags = %v", tags)
	}
	list, _ := srv.Find("GET /inference-profiles")
	if list.Query.Get("type") != "APPLICATION" {
		t.Errorf("list query = %v", list.Query)
	}
}

// M1-G2c: an existing profile is returned, not created again, even on a later page.
func TestEnsureProfileIsIdempotent(t *testing.T) {
	c, srv := newControl(t)
	srv.On(listHandler(
		map[string]any{"inferenceProfileSummaries": []any{summary("afs-other", "arn:other")}, "nextToken": "p2"},
		map[string]any{"inferenceProfileSummaries": []any{summary(pname, profARN)}},
	))
	want := spec()
	want.Tags[team.TagManagedBy] = team.ManagedByValue
	srv.On(tagsHandler(want.Tags))
	p, err := c.EnsureProfile(t.Context(), spec())
	if err != nil || p.ARN != profARN || p.SourceARN != srcARN {
		t.Fatalf("EnsureProfile = %+v, %v", p, err)
	}
	for _, r := range srv.Requests() {
		if r.Method == "POST" && !strings.HasPrefix(r.Path, "/listTagsForResource") {
			t.Errorf("an existing profile with the right tags was written: %s %s", r.Method, r.Path)
		}
	}
}

// tagsHandler answers ListTagsForResource with have.
func tagsHandler(have map[string]string) awstest.Handler {
	return func(r awstest.Request) *awstest.Response {
		if r.Method != "POST" || !strings.HasPrefix(r.Path, "/listTagsForResource") {
			return nil
		}
		var tags []map[string]string
		for k, v := range have {
			tags = append(tags, map[string]string{"key": k, "value": v})
		}
		resp := awstest.JSONOK(map[string]any{"tags": tags})
		return &resp
	}
}

// M4-B1 / RC-9: an existing profile gets changed tags, and loses the tags AFS applied that are no longer
// wanted, but never tags somebody else put there; a profile we did not create is left alone.
func TestEnsureProfileSyncsTags(t *testing.T) {
	c, srv := newControl(t)
	srv.On(listHandler(map[string]any{"inferenceProfileSummaries": []any{summary(pname, profARN)}}))
	srv.On(tagsHandler(map[string]string{
		team.TagManagedBy: team.ManagedByValue, team.TagTeamID: "team_01JD0000000000000000000001", team.TagTeamSlug: "old-slug",
		"cost-center": "cc-1", "afs-tag-keys": "cost-center", "foreign": "keep", "afs-agent-name": "gone",
	}))
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Method == "POST" && (r.Path == "/tagResource" || r.Path == "/untagResource") {
			resp := awstest.JSONOK(map[string]any{})
			return &resp
		}
		return nil
	})
	sp := spec()
	sp.Tags["afs-tag-keys"] = "project"
	sp.Tags["project"] = "apollo"
	if _, err := c.EnsureProfile(t.Context(), sp); err != nil {
		t.Fatal(err)
	}
	tag, ok := srv.Find("POST /tagResource")
	if !ok || !strings.Contains(string(tag.Body), "apollo") || !strings.Contains(string(tag.Body), "payments") {
		t.Errorf("tagResource = %s", tag.Body)
	}
	untag, ok := srv.Find("POST /untagResource")
	if !ok {
		t.Fatal("stale tags were not removed")
	}
	body := string(untag.Body)
	if !strings.Contains(body, "cost-center") || !strings.Contains(body, "afs-agent-name") || strings.Contains(body, "foreign") || strings.Contains(body, team.TagManagedBy) {
		t.Errorf("untagResource = %s", body)
	}
}

func TestEnsureProfileLeavesAForeignProfileAlone(t *testing.T) {
	c, srv := newControl(t)
	srv.On(listHandler(map[string]any{"inferenceProfileSummaries": []any{summary(pname, profARN)}}))
	srv.On(tagsHandler(map[string]string{"owner": "someone"}))
	if _, err := c.EnsureProfile(t.Context(), spec()); err != nil {
		t.Fatal(err)
	}
	for _, r := range srv.Requests() {
		if r.Path == "/tagResource" || r.Path == "/untagResource" {
			t.Errorf("a profile we did not create was retagged: %s", r.Path)
		}
	}
}

func TestEnsureProfileRefusesOutsideNamespaceAndMissingSource(t *testing.T) {
	c, _ := newControl(t)
	if _, err := c.EnsureProfile(t.Context(), team.ProfileSpec{Name: "payments-profile", SourceARN: srcARN}); !errors.Is(err, ErrNameNotOurs) || !errors.Is(err, engine.ErrTerminal) {
		t.Errorf("name outside afs-: %v", err)
	}
	if _, err := c.EnsureProfile(t.Context(), team.ProfileSpec{Name: pname}); !errors.Is(err, engine.ErrTerminal) {
		t.Errorf("no source: %v", err)
	}
}

func TestEnsureProfileErrorsCarryCodes(t *testing.T) {
	c, srv := newControl(t)
	srv.On(listHandler(map[string]any{"inferenceProfileSummaries": []any{}}))
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Method == "POST" {
			resp := awstest.JSONError(403, "AccessDeniedException", "not allowed to bedrock:CreateInferenceProfile")
			return &resp
		}
		return nil
	})
	_, err := c.EnsureProfile(t.Context(), spec())
	if engine.CodeOf(err) != "AccessDeniedException" {
		t.Errorf("code = %q, err = %v", engine.CodeOf(err), err)
	}
	c2, srv2 := newControl(t)
	srv2.On(func(r awstest.Request) *awstest.Response {
		resp := awstest.JSONError(429, "ThrottlingException", "slow down")
		return &resp
	})
	if _, err := c2.EnsureProfile(t.Context(), spec()); engine.CodeOf(err) != "ThrottlingException" {
		t.Errorf("list error code = %q", engine.CodeOf(err))
	}
}

// M1-G2c: profiles are found by team tag, and only ones the reconciler created.
func TestListProfilesByTag(t *testing.T) {
	c, srv := newControl(t)
	srv.On(listHandler(map[string]any{"inferenceProfileSummaries": []any{
		summary("afs-a", "arn:a"), summary("afs-b", "arn:b"), summary("afs-c", "arn:c"), summary("other", "arn:o"),
	}}))
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Method != "POST" || !strings.HasPrefix(r.Path, "/listTagsForResource") {
			return nil
		}
		var in struct {
			ResourceARN string `json:"resourceARN"`
		}
		_ = r.JSON(&in)
		tags := map[string][]map[string]string{
			"arn:a": {{"key": team.TagManagedBy, "value": team.ManagedByValue}, {"key": team.TagTeamID, "value": "team_X"}},
			"arn:b": {{"key": team.TagManagedBy, "value": team.ManagedByValue}, {"key": team.TagTeamID, "value": "team_Y"}},
			"arn:c": {{"key": team.TagTeamID, "value": "team_X"}}, // tagged for the team but not created by us
		}
		resp := awstest.JSONOK(map[string]any{"tags": tags[in.ResourceARN]})
		return &resp
	})
	got, err := c.ListProfiles(t.Context(), team.TagTeamID, "team_X")
	if err != nil || len(got) != 1 || got[0].Name != "afs-a" {
		t.Errorf("ListProfiles = %+v, %v", got, err)
	}
}

func TestDeleteProfile(t *testing.T) {
	c, srv := newControl(t)
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Method == "DELETE" && strings.HasSuffix(r.Path, "/"+url.PathEscape("abc123")) {
			resp := awstest.JSONOK(map[string]any{})
			return &resp
		}
		if r.Method == "DELETE" {
			resp := awstest.JSONError(404, "ResourceNotFoundException", "gone")
			return &resp
		}
		return nil
	})
	if err := c.DeleteProfile(t.Context(), "abc123"); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := c.DeleteProfile(t.Context(), "gone"); err != nil {
		t.Errorf("deleting a missing profile: %v", err)
	}
	c2, srv2 := newControl(t)
	srv2.On(func(r awstest.Request) *awstest.Response {
		resp := awstest.JSONError(403, "AccessDeniedException", "no")
		return &resp
	})
	if err := c2.DeleteProfile(t.Context(), "abc123"); engine.CodeOf(err) != "AccessDeniedException" {
		t.Errorf("delete error code = %q", engine.CodeOf(err))
	}
}

// RC-US-15.3: a deleted team's inference profiles are removed, and only that team's, only ours.
func TestDeleteTeamProfiles(t *testing.T) {
	c, srv := newControl(t)
	srv.On(listHandler(map[string]any{"inferenceProfileSummaries": []any{summary("afs-a", "arn:a"), summary("afs-b", "arn:b")}}))
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Method != "POST" || !strings.HasPrefix(r.Path, "/listTagsForResource") {
			return nil
		}
		var in struct {
			ResourceARN string `json:"resourceARN"`
		}
		_ = r.JSON(&in)
		team := "team_X"
		if in.ResourceARN == "arn:b" {
			team = "team_Y"
		}
		resp := awstest.JSONOK(map[string]any{"tags": []map[string]string{{"key": "afs-managed-by", "value": "afs-reconciler"}, {"key": "afs-team-id", "value": team}}})
		return &resp
	})
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Method == "DELETE" {
			resp := awstest.JSONOK(map[string]any{})
			return &resp
		}
		return nil
	})
	if err := c.DeleteTeamProfiles(t.Context(), "team_X"); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	for _, r := range srv.Requests() {
		if r.Method == "DELETE" {
			deleted = append(deleted, r.Path)
		}
	}
	if len(deleted) != 1 || !strings.Contains(deleted[0], "arn:a") && !strings.Contains(deleted[0], "arn%3Aa") {
		t.Errorf("deleted = %v, want only team_X's profile", deleted)
	}
}
