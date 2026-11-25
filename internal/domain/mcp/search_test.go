package mcp_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

func corpus() []mcp.Primitive {
	return []mcp.Primitive{
		{Server: "github", Kind: mcp.KindTool, Name: "create_pull_request", Title: "Create pull request", Description: "Open a pull request from a branch.", Params: []string{"owner", "repo", "head", "base"}},
		{Server: "github", Kind: mcp.KindTool, Name: "list_issues", Title: "List issues", Description: "List issues in a repository.", Params: []string{"owner", "repo", "state"}},
		{Server: "github", Kind: mcp.KindTool, Name: "get_file_contents", Description: "Read a file from a repository.", Params: []string{"path"}},
		{Server: "jira", Kind: mcp.KindTool, Name: "searchIssues", Title: "Search issues", Description: "Search Jira issues with JQL."},
		{Server: "jira", Kind: mcp.KindTool, Name: "createIssue", Description: "Create a Jira issue."},
		{Server: "wiki", Kind: mcp.KindResourceTemplate, Name: "wiki://pages/{id}", Title: "Wiki page", Description: "A wiki page by id."},
		{Server: "wiki", Kind: mcp.KindPrompt, Name: "summarize_page", Description: "Summarize a wiki page."},
	}
}

func names(hs []mcp.Hit) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Primitive.QualifiedName()
	}
	return out
}

func TestSearchRanks(t *testing.T) {
	ix := mcp.NewIndex(corpus())
	if ix.Len() != 7 {
		t.Fatalf("Len = %d", ix.Len())
	}
	hs := ix.Search(mcp.Query{Text: "open a pull request"})
	if len(hs) == 0 || hs[0].Primitive.Name != "create_pull_request" {
		t.Fatalf("pull request: %v", names(hs))
	}
	hs = ix.Search(mcp.Query{Text: "issues", Servers: []string{"jira"}})
	if got := names(hs); !reflect.DeepEqual(got, []string{"jira__searchIssues", "jira__createIssue"}) {
		t.Errorf("jira issues: %v", got)
	}
	hs = ix.Search(mcp.Query{Text: "wiki page", Kinds: []mcp.Kind{mcp.KindResource}})
	if got := names(hs); !reflect.DeepEqual(got, []string{"wiki__wiki://pages/{id}"}) {
		t.Errorf("resource kind should include templates: %v", got)
	}
	hs = ix.Search(mcp.Query{Text: "wiki page", Kinds: []mcp.Kind{mcp.KindPrompt}})
	if got := names(hs); !reflect.DeepEqual(got, []string{"wiki__summarize_page"}) {
		t.Errorf("prompt kind: %v", got)
	}
	if hs := ix.Search(mcp.Query{Text: "the of a"}); hs != nil {
		t.Errorf("stop words only: %v", names(hs))
	}
	if hs := ix.Search(mcp.Query{Text: "kubernetes"}); len(hs) != 0 {
		t.Errorf("no match: %v", names(hs))
	}
}

func TestSearchFiltersAndBoost(t *testing.T) {
	ix := mcp.NewIndex(corpus())
	deny := func(p mcp.Primitive) bool { return p.Server != "github" }
	for _, h := range ix.Search(mcp.Query{Text: "issues", Allowed: deny}) {
		if h.Primitive.Server == "github" {
			t.Errorf("disallowed primitive returned: %s", h.Primitive.QualifiedName())
		}
	}
	base := ix.Search(mcp.Query{Text: "create"})
	boosted := ix.Search(mcp.Query{Text: "create", Boost: map[string]float64{"jira": 5, "github": 0.1}})
	if boosted[0].Primitive.Server != "jira" {
		t.Errorf("boost: base %v boosted %v", names(base), names(boosted))
	}
}

func TestSearchLimitsAndDeterminism(t *testing.T) {
	var ps []mcp.Primitive
	for i := 0; i < 40; i++ {
		ps = append(ps, mcp.Primitive{Server: "s", Kind: mcp.KindTool, Name: fmt.Sprintf("tool_%02d", 39-i), Description: "Same text."})
	}
	ix := mcp.NewIndex(ps)
	if n := len(ix.Search(mcp.Query{Text: "same"})); n != mcp.DefaultSearchLimit {
		t.Errorf("default limit: %d", n)
	}
	if n := len(ix.Search(mcp.Query{Text: "same", Limit: 100})); n != mcp.MaxSearchLimit {
		t.Errorf("max limit: %d", n)
	}
	hs := ix.Search(mcp.Query{Text: "same", Limit: 3})
	if got := names(hs); !reflect.DeepEqual(got, []string{"s__tool_00", "s__tool_01", "s__tool_02"}) {
		t.Errorf("tiebreak by name: %v", got)
	}
	if empty := mcp.NewIndex(nil); empty.Len() != 0 || empty.Search(mcp.Query{Text: "x y"}) != nil {
		t.Error("empty index")
	}
}

func TestTokenize(t *testing.T) {
	tests := map[string][]string{
		"create_pull_request": {"create", "pull", "request"},
		"searchIssues":        {"search", "issue"},
		"HTTPServer":          {"http", "server"},
		"get-file-contents":   {"get", "file", "content"},
		"The class of a Bus":  {"class", "bus"},
		"x y z":               nil,
		"wiki://pages/{id}":   {"wiki", "page", "id"},
		"ISSUES v2":           {"issue", "v2"},
	}
	for in, want := range tests {
		if got := mcp.Tokenize(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Tokenize(%q) = %v want %v", in, got, want)
		}
	}
}
