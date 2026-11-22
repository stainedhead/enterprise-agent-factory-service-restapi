package collab

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestScanFindsEveryPatternType(t *testing.T) {
	samples := map[string]string{ //nolint:gosec // fake credentials to prove the scanner
		PatternAWSAccessKey:      "key AKIAIOSFODNN7EXAMPLE here",
		PatternGitHubToken:       "token ghp_" + strings.Repeat("a", 36),
		PatternSlackToken:        "xoxb-1234567890-abcdefghij",
		PatternPrivateKey:        "-----BEGIN RSA PRIVATE KEY-----\nabc",
		PatternJWT:               "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijkl",
		PatternOktaClientSecret:  `okta_client_secret = "` + strings.Repeat("A", 40) + `"`,
		PatternEntraClientSecret: "abc8Q~" + strings.Repeat("x", 34),
		PatternGenericAssignment: `password: "hunter2hunter2"`,
	}
	if len(samples) != len(PatternTypes()) {
		t.Fatal("a pattern type has no sample")
	}
	for name, in := range samples {
		got := Scan([]byte(in))
		found := false
		for _, m := range got {
			if m.Pattern == name && m.Count >= 1 {
				found = true
			}
		}
		if !found {
			t.Errorf("%s not found in %q: %v", name, in, got)
		}
	}
	for _, clean := range []string{"", "hello world", `{"name":"report","count":3}`, "AKIA short"} {
		if m := Scan([]byte(clean)); len(m) != 0 {
			t.Errorf("false positive on %q: %v", clean, m)
		}
	}
}

func TestStreamFindsMatchSpanningChunksAndCountsOnce(t *testing.T) {
	key := "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // fake credential
	body := strings.Repeat("x ", WindowBytes/2-5) + key + strings.Repeat(" y", WindowBytes)
	// Feed in odd chunk sizes so the key straddles chunk and window boundaries.
	for _, size := range []int{1, 7, 1000, WindowBytes - 3, WindowBytes + 100} {
		s := NewStream()
		b := []byte(body)
		for len(b) > 0 {
			n := min(size, len(b))
			s.Write(b[:n])
			b = b[n:]
		}
		got := s.Finish()
		if len(got) != 1 || got[0].Pattern != PatternAWSAccessKey || got[0].Count != 1 {
			t.Errorf("chunk %d: %v", size, got)
		}
	}
	// A key placed across the window limit is counted exactly once.
	for off := WindowBytes - 25; off < WindowBytes+5; off++ {
		pad := strings.Repeat("a ", off/2)
		s := NewStream()
		s.Write([]byte(pad + key + strings.Repeat(" b", 2*OverlapBytes+WindowBytes)))
		if got := s.Finish(); len(got) != 1 || got[0].Count != 1 {
			t.Fatalf("offset %d: %v", off, got)
		}
	}
	// Two patterns come back in the fixed order.
	s := NewStream()
	s.Write([]byte(`password: "hunter2hunter2" and ` + key))
	got := s.Finish()
	if len(got) != 2 || got[0].Pattern != PatternAWSAccessKey || got[1].Pattern != PatternGenericAssignment {
		t.Errorf("order: %v", got)
	}
}

func TestTextContentType(t *testing.T) {
	for ct, want := range map[string]bool{"": true, "application/json": true, "text/plain": true, "application/vnd.api+json": true,
		"image/png": false, "application/octet-stream": false, "multipart/form-data": true, "application/atom+xml": true} {
		if TextContentType(ct) != want {
			t.Errorf("%q: want %v", ct, want)
		}
	}
}

func route() Route {
	return Route{TeamID: "team_a", Slug: "a", Namespace: "afs-t-a", Hostname: "a.collab.example.com", ServiceName: "paperclip", Port: 3100,
		WebUIEffective: true, RESTAPIEffective: true, Ready: true}
}

func has(perms ...string) Principal {
	return Principal{HasBinding: true, Has: func(p string) bool {
		for _, x := range perms {
			if x == p {
				return true
			}
		}
		return false
	}}
}

func TestDecide(t *testing.T) {
	off := route()
	off.WebUIEffective, off.RESTAPIEffective = false, false
	apiOnly := route()
	apiOnly.WebUIEffective = false
	starting := route()
	starting.Ready, starting.NotReadyReason = false, ""
	paused := route()
	paused.Ready, paused.NotReadyReason = false, "paused"
	tests := []struct {
		name   string
		r      Route
		c      Class
		method string
		p      Principal
		status int
		reason string
		req    string
	}{
		{"viewer reads web", route(), ClassWeb, "GET", has(PermAccess), 200, "", ""},
		{"viewer HEAD", route(), ClassWeb, "HEAD", has(PermAccess), 200, "", ""},
		{"viewer cannot write", route(), ClassWeb, "POST", has(PermAccess), 403, "permission_required", PermContribute},
		{"collaborator writes", route(), ClassWeb, "PATCH", has(PermAccess, PermContribute), 200, "", ""},
		{"api needs api", route(), ClassAPI, "GET", has(PermAccess), 403, "permission_required", PermAPI},
		{"api write needs contribute", route(), ClassAPI, "POST", has(PermAPI), 403, "permission_required", PermContribute},
		{"api write ok", route(), ClassAPI, "POST", has(PermAPI, PermContribute), 200, "", ""},
		{"api read ok", route(), ClassAPI, "GET", has(PermAPI, PermAccess), 200, "", ""},
		{"both off is 404", off, ClassWeb, "GET", has(PermAccess), 404, "", ""},
		{"no binding", route(), ClassWeb, "GET", Principal{Has: func(string) bool { return false }}, 403, "no_team_role", ""},
		{"surface disabled", apiOnly, ClassWeb, "GET", has(PermAccess), 403, "surface_disabled", ""},
		{"starting", starting, ClassWeb, "GET", has(PermAccess), 503, "starting", ""},
		{"paused", paused, ClassWeb, "GET", has(PermAccess), 503, "paused", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.r, tt.c, tt.method, tt.p)
			if d.Status != tt.status || d.Reason != tt.reason || d.Required != tt.req || d.Allow != (tt.status == 200) {
				t.Errorf("%+v", d)
			}
			if tt.status == 503 && d.RetryAfter != 30 {
				t.Error("no Retry-After")
			}
		})
	}
	if !FrameAllowed(has(PermContribute)) || FrameAllowed(has(PermAccess)) || FrameAllowed(Principal{}) {
		t.Error("frame rule")
	}
}

func TestOutcomeOf(t *testing.T) {
	for d, want := range map[Decision]string{
		Allowed:                                OutcomeAllowed,
		{Status: 401, Code: "Unauthenticated"}: OutcomeDeniedUnauth,
		{Status: 400}:                          OutcomeDeniedBad,
		{Status: 429}:                          OutcomeThrottled,
		{Status: 503}:                          OutcomeUnavailable,
		{Status: 403, Reason: "surface_disabled"}:    OutcomeDeniedSurface,
		{Status: 422, Code: "SecretDetected"}:        OutcomeDeniedSecret,
		{Status: 403, Reason: "permission_required"}: OutcomeDeniedForbid,
	} {
		if got := OutcomeOf(d); got != want {
			t.Errorf("%+v: %s want %s", d, got, want)
		}
	}
	if (ClassAPI).Surface() != SurfaceRESTAPI || (ClassWeb).Surface() != SurfaceWebUI || !Safe(http.MethodGet) || Safe("PUT") {
		t.Error("class or method")
	}
}

func TestTable(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	clock := now
	tb := NewTable(func() time.Time { return clock })
	if tb.Fresh() {
		t.Error("empty table is fresh")
	}
	s := DefaultSettings()
	s.BaseDomain = "collab.example.com"
	tb.Replace([]Route{route(), {TeamID: "team_none"}}, s)
	if !tb.Fresh() {
		t.Error("loaded table not fresh")
	}
	if r, k := tb.Lookup("A.collab.example.com:8443"); k != HostTeam || r.TeamID != "team_a" {
		t.Errorf("team host: %v %v", r, k)
	}
	if _, k := tb.Lookup("collab.example.com"); k != HostApex {
		t.Error("apex")
	}
	if _, k := tb.Lookup("evil.example.org"); k != HostUnknown {
		t.Error("unknown host")
	}
	if _, ok := tb.RouteByTeam("team_a"); !ok {
		t.Error("RouteByTeam")
	}
	if _, ok := tb.RouteByTeam("team_x"); ok {
		t.Error("RouteByTeam unknown")
	}
	if tb.Settings().BaseDomain != "collab.example.com" || tb.Settings().MaxBodyBytes() != 25<<20 {
		t.Error("settings")
	}
	if got := route().Upstream(); got != "http://paperclip.afs-t-a.svc.cluster.local:3100" {
		t.Errorf("upstream %s", got)
	}
	clock = now.Add(MaxStale + time.Second)
	if tb.Fresh() {
		t.Error("stale table is fresh")
	}
}

func TestAccessBuffer(t *testing.T) {
	b := NewAccessBuffer()
	at := time.Date(2026, 11, 22, 12, 7, 31, 0, time.UTC)
	k := AccessKey{TeamID: "team_a", Surface: SurfaceWebUI, Kind: KindUser, PrincipalID: "usr_1", Outcome: OutcomeAllowed}
	b.Add(at, k, 100)
	b.Add(at.Add(time.Second), k, 50)
	b.Add(at.Add(6*time.Minute), k, 1)
	if b.Len() != 2 {
		t.Fatalf("keys = %d", b.Len())
	}
	rows := b.Take()
	if len(rows) != 2 || b.Len() != 0 {
		t.Fatal("take")
	}
	for _, r := range rows {
		if r.Key.Bucket.Unix()%BucketSeconds != 0 {
			t.Error("bucket not aligned")
		}
		if r.Requests == 2 && r.Bytes != 150 {
			t.Errorf("sum %+v", r)
		}
	}
	b.Restore(rows)
	b.Restore(rows)
	if rows2 := b.Take(); len(rows2) != 2 {
		t.Error("restore")
	}
	for i := 0; i < MaxBufferedKeys+10; i++ {
		kk := k
		kk.PrincipalID = string(rune('a'+i%26)) + string(rune(i))
		kk.Bucket = time.Unix(int64(i), 0)
		b.Add(at.Add(time.Duration(i)*time.Hour), AccessKey{TeamID: "t", Surface: SurfaceWebUI, Kind: KindUser, PrincipalID: kk.PrincipalID + time.Duration(i).String(), Outcome: OutcomeAllowed}, 1)
	}
	if b.Len() != MaxBufferedKeys || b.Dropped() != 10 {
		t.Errorf("cap: len=%d dropped=%d", b.Len(), b.Dropped())
	}
	if got := len(b.Take()); got != MaxFlushRows {
		t.Errorf("take = %d", got)
	}
	b.Restore([]Row{{Key: AccessKey{TeamID: "new"}, Access: Access{Requests: 3}}})
}

func TestLimiter(t *testing.T) {
	clock := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(20, 40, func() time.Time { return clock })
	for i := 0; i < 40; i++ {
		if ok, _ := l.Allow("p"); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	ok, wait := l.Allow("p")
	if ok || wait < 1 {
		t.Errorf("41st: %v %d", ok, wait)
	}
	if ok, _ := l.Allow("other"); !ok {
		t.Error("another principal shares the bucket")
	}
	clock = clock.Add(time.Second)
	for i := 0; i < 20; i++ {
		if ok, _ := l.Allow("p"); !ok {
			t.Fatalf("refill request %d refused", i)
		}
	}
	clock = clock.Add(time.Hour)
	l.Sweep(time.Minute)
	if len(l.buckets) != 0 {
		t.Error("sweep")
	}
	clock = clock.Add(time.Hour)
	l.Allow("x")
	clock = clock.Add(time.Hour)
	if ok, _ := l.Allow("x"); !ok {
		t.Error("capped refill")
	}
}
