package mcp_test

import (
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

func TestCanonicalize(t *testing.T) {
	tests := []struct{ in, want string }{
		// RFC 8785 §3.2.2 example.
		{`{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001],"string":"\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/","literals":[null,true,false]}`,
			`{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`},
		// RFC 8785 §3.2.3 sorting by UTF-16 code units.
		{`{"\u20ac":"Euro Sign","\r":"Carriage Return","\ufb33":"Hebrew Letter Dalet With Dagesh","1":"One","\ud83d\ude00":"Emoji: Grinning Face","\u0080":"Control","\u00f6":"Latin Small Letter O With Diaeresis"}`,
			"{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"\u00f6\":\"Latin Small Letter O With Diaeresis\",\"\u20ac\":\"Euro Sign\",\"\U0001F600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}"},
		{` { "b" : [ 1 , { "d":0,"c":-0 } ], "a" : "" } `, `{"a":"","b":[1,{"c":0,"d":0}]}`},
		{`1e21`, `1e+21`}, {`1e20`, `100000000000000000000`}, {`123e-9`, `1.23e-7`}, {`0.000001`, `0.000001`},
		{`-1.5`, `-1.5`}, {`"\b\f\t\u0001"`, `"\b\f\t\u0001"`}, {`"<&>\u2028"`, "\"<&>\u2028\""},
	}
	for _, tt := range tests {
		got, err := mcp.Canonicalize([]byte(tt.in))
		if err != nil {
			t.Errorf("%s: %v", tt.in, err)
			continue
		}
		if string(got) != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.in, got, tt.want)
		}
	}
}

func TestCanonicalizeRejects(t *testing.T) {
	for _, in := range []string{``, `{`, `{"a":1} {"b":2}`, `1e400`, `[1,]`, `{"a":1}x`} {
		if _, err := mcp.Canonicalize([]byte(in)); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestDigest(t *testing.T) {
	a, err := mcp.Digest([]byte(`{"name":"x","inputSchema":{"type":"object","properties":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := mcp.Digest([]byte(`{"inputSchema":{"properties":{},"type":"object"},  "name":"x"}`))
	if a != b || !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+64 {
		t.Errorf("digest %s %s", a, b)
	}
	c, _ := mcp.Digest([]byte(`{"name":"y","inputSchema":{"type":"object","properties":{}}}`))
	if c == a {
		t.Error("different definitions share a digest")
	}
	if _, err := mcp.Digest([]byte(`nope`)); err == nil {
		t.Error("want error")
	}
}
