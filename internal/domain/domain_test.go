package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

var now = time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)

func TestIDs(t *testing.T) {
	if got := len(domain.AllIDPrefixes()); got != 28 {
		t.Fatalf("prefixes = %d, want 28 (27 of contract §1 plus tpl)", got)
	}
	for _, p := range domain.AllIDPrefixes() {
		id, err := domain.NewID(p, now, nil)
		if err != nil {
			t.Fatal(err)
		}
		if id.Prefix() != p {
			t.Errorf("prefix of %s = %s", id, id.Prefix())
		}
		if _, err := domain.ParseID(p, id.String()); err != nil {
			t.Errorf("parse %s: %v", id, err)
		}
	}
	bad := []struct {
		p domain.IDPrefix
		s string
	}{
		{domain.PrefixTeam, "agt_01JD0000000000000000000000"},
		{domain.PrefixTeam, "team_01jd0000000000000000000000"},
		{domain.PrefixTeam, "team_01JD00000000000000000000"},
		{domain.PrefixTeam, "team_01JD00000000000000000000IL"},
		{domain.PrefixTeam, "team01JD0000000000000000000000"},
	}
	for _, c := range bad {
		if _, err := domain.ParseID(c.p, c.s); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("ParseID(%s, %q) err = %v, want ValidationError", c.p, c.s, err)
		}
	}
	if domain.ID("nounderscore").Prefix() != "" {
		t.Error("prefix of an id without underscore should be empty")
	}
	if _, err := domain.NewID(domain.PrefixTeam, now, bytes.NewReader(nil)); err == nil {
		t.Error("NewID with exhausted entropy should fail")
	}
}

func TestNames(t *testing.T) {
	for s, ok := range map[string]bool{"ab": false, "abc": true, "a-b-c": true, "1ab": false, "ab-": false, "Abc": false, strings.Repeat("a", 40): true, strings.Repeat("a", 41): false} {
		_, err := domain.ParseName("slug", s)
		if (err == nil) != ok {
			t.Errorf("ParseName(%q) err = %v, want ok=%v", s, err, ok)
		}
	}
	for s, ok := range map[string]bool{"LOG_LEVEL": true, "log_level": false, "DB_PASSWORD": false, "GITHUB_TOKEN": false, "AWS_ACCESS_KEY_ID": false, "A": true, "_A": false} {
		_, err := domain.ParseConfigName("name", s)
		if (err == nil) != ok {
			t.Errorf("ParseConfigName(%q) err = %v, want ok=%v", s, err, ok)
		}
	}
	d := strings.Repeat("a", 64)
	for s, ok := range map[string]bool{"ghcr.io/x/y@sha256:" + d: true, "ghcr.io/x/y:latest": false, "@sha256:" + d: false, "x@sha256:" + d[:63]: false} {
		_, err := domain.ParseImageRef("image", s)
		if (err == nil) != ok {
			t.Errorf("ParseImageRef(%q) err = %v, want ok=%v", s, err, ok)
		}
	}
}

func TestMoney(t *testing.T) {
	cases := []struct{ in, out string }{
		{"0", "0.00"}, {"1234.5", "1234.50"}, {"1234.50", "1234.50"}, {"0.000123", "0.000123"},
		{"-2.25", "-2.25"}, {"999999999999.999999", "999999999999.999999"},
	}
	for _, c := range cases {
		m, err := domain.ParseMoney("amountUsd", c.in)
		if err != nil {
			t.Fatalf("ParseMoney(%q): %v", c.in, err)
		}
		if m.String() != c.out {
			t.Errorf("ParseMoney(%q) = %s, want %s", c.in, m, c.out)
		}
	}
	for _, s := range []string{"", "1.2345678", "1e3", "01", "1.", ".5", "1,5", "1000000000000"} {
		if _, err := domain.ParseMoney("amountUsd", s); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("ParseMoney(%q) err = %v, want ValidationError", s, err)
		}
	}
	a, b := domain.MustMoney("10.50"), domain.MustMoney("0.25")
	if a.Add(b).String() != "10.75" || a.Sub(b).String() != "10.25" || a.Cmp(b) != 1 || b.Cmp(a) != -1 || a.Cmp(a) != 0 {
		t.Error("arithmetic")
	}
	if !b.Sub(a).IsNegative() || a.IsNegative() || domain.MoneyFromMicros(5).Micros() != 5 {
		t.Error("sign")
	}
	type wrap struct {
		Amount domain.Money `json:"amountUsd"`
	}
	raw, _ := json.Marshal(wrap{a})
	if string(raw) != `{"amountUsd":"10.50"}` {
		t.Errorf("JSON = %s, want a string, never a number", raw)
	}
	var w wrap
	if err := json.Unmarshal([]byte(`{"amountUsd":"3.1"}`), &w); err != nil || w.Amount.String() != "3.10" {
		t.Errorf("unmarshal = %v, %v", w.Amount, err)
	}
	if err := json.Unmarshal([]byte(`{"amountUsd":"x"}`), &w); err == nil {
		t.Error("unmarshal of a bad amount should fail")
	}
	defer func() {
		if recover() == nil {
			t.Error("MustMoney should panic")
		}
	}()
	domain.MustMoney("x")
}

func TestStatus(t *testing.T) {
	for _, p := range domain.Phases() {
		if !p.Valid() {
			t.Errorf("%s invalid", p)
		}
	}
	if domain.Phase("Running").Valid() || !domain.PhaseReady.Terminal() || domain.PhasePending.Terminal() {
		t.Error("phase rules")
	}
	s := domain.PendingStatus(now)
	if s.Phase != domain.PhasePending || s.Conditions == nil || s.BudgetBlocked() {
		t.Errorf("pending = %+v", s)
	}
	s.Phase = domain.PhaseReady
	s.Conditions = append(s.Conditions, domain.Condition{Type: domain.CondBudgetExhausted, Status: domain.ConditionTrue})
	if !s.BudgetBlocked() {
		t.Error("BudgetBlocked")
	}
	if _, ok := s.Condition(domain.CondIamReady); ok {
		t.Error("absent condition found")
	}
}

func TestConcurrency(t *testing.T) {
	if domain.Version(7).ETag() != `"7"` {
		t.Error("etag")
	}
	for in, want := range map[string]domain.Version{`"7"`: 7, "7": 7, `W/"12"`: 12, ` "3" `: 3} {
		v, err := domain.ParseIfMatch(in)
		if err != nil || v != want {
			t.Errorf("ParseIfMatch(%q) = %d, %v", in, v, err)
		}
	}
	if _, err := domain.ParseIfMatch(""); domain.KindOf(err) != domain.KindPreconditionRequired {
		t.Errorf("empty If-Match err = %v", err)
	}
	for _, in := range []string{`"x"`, "0", "-1", `"1", "2"`, "*"} {
		if _, err := domain.ParseIfMatch(in); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("ParseIfMatch(%q) err = %v", in, err)
		}
	}
	if domain.CheckVersion(3, 3) != nil || domain.KindOf(domain.CheckVersion(3, 2)) != domain.KindPreconditionFailed {
		t.Error("CheckVersion")
	}
}

func TestRequestID(t *testing.T) {
	if got := domain.AcceptRequestID("abc-123", now, nil); got != "abc-123" {
		t.Errorf("echo = %s", got)
	}
	for _, in := range []string{"", strings.Repeat("a", 65), "has space", "tab\t", "ünï"} {
		got := domain.AcceptRequestID(in, now, nil)
		if string(got) == in || !strings.HasPrefix(string(got), "req_") {
			t.Errorf("AcceptRequestID(%q) = %s, want a generated id", in, got)
		}
	}
	if got := domain.AcceptRequestID(strings.Repeat("a", 64), now, nil); len(got) != 64 {
		t.Error("64 characters must be echoed")
	}
	if got := domain.NewRequestID(now, bytes.NewReader(nil)); !strings.HasPrefix(string(got), "req_") {
		t.Errorf("fallback id = %s", got)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		err  *domain.Error
		kind domain.ErrorKind
	}{
		{domain.Validation(domain.FieldError{Field: "slug", Reason: "bad"}), domain.KindValidation},
		{domain.Validation(), domain.KindValidation},
		{domain.NotFound("team"), domain.KindNotFound},
		{domain.Conflict(domain.ReasonTeamSuspended, "team is suspended", nil), domain.KindConflict},
		{domain.PreconditionFailed(), domain.KindPreconditionFailed},
		{domain.PreconditionRequired(), domain.KindPreconditionRequired},
		{domain.AccessDenied("teams:write"), domain.KindAccessDenied},
		{domain.Unauthenticated(), domain.KindUnauthenticated},
		{domain.Unavailable("database", errors.New("dial")), domain.KindUnavailable},
		{domain.Internal(errors.New("boom")), domain.KindInternal},
	}
	for _, c := range cases {
		wrapped := fmt.Errorf("ctx: %w", c.err)
		if domain.KindOf(wrapped) != c.kind || c.err.Error() == "" {
			t.Errorf("%v kind = %s, want %s", c.err, domain.KindOf(wrapped), c.kind)
		}
		if !errors.Is(wrapped, &domain.Error{Kind: c.kind}) {
			t.Errorf("errors.Is by kind failed for %s", c.kind)
		}
	}
	if domain.KindOf(nil) != "" || domain.KindOf(errors.New("x")) != domain.KindInternal {
		t.Error("KindOf of nil or plain errors")
	}
	c := domain.Conflict(domain.ReasonIdempotencyMismatch, "m", nil)
	if errors.Is(c, &domain.Error{Kind: domain.KindConflict, Reason: domain.ReasonTeamSuspended}) {
		t.Error("reason must match when given")
	}
	if errors.Is(c, errors.New("x")) {
		t.Error("plain errors never match")
	}
	if !errors.Is(domain.Internal(errBoom), errBoom) {
		t.Error("Unwrap")
	}
	if domain.AccessDenied("x:y").RequiredPermission != "x:y" {
		t.Error("required permission")
	}
}

var errBoom = errors.New("boom")

func TestEnums(t *testing.T) {
	if len(domain.EnumValues) < 40 {
		t.Fatalf("enums = %d", len(domain.EnumValues))
	}
	if !domain.TeamKindEphemeral.Valid() || domain.TeamKind("x").Valid() || !domain.ActorTypePool.Valid() {
		t.Error("Valid")
	}
	for name, vs := range domain.EnumValues {
		seen := map[string]bool{}
		for _, v := range vs {
			if seen[v] {
				t.Errorf("%s repeats %s", name, v)
			}
			seen[v] = true
		}
	}
}

func TestULIDEncoding(t *testing.T) {
	// The zero-entropy id at the Unix epoch is all zeros; the max time first char is 7.
	zero := bytes.NewReader(make([]byte, 10))
	id, err := domain.NewID(domain.PrefixTeam, time.UnixMilli(0), zero)
	if err != nil || id != "team_00000000000000000000000000" {
		t.Fatalf("zero id = %s, %v", id, err)
	}
	ff := bytes.NewReader(bytes.Repeat([]byte{0xff}, 10))
	id, _ = domain.NewID(domain.PrefixTeam, time.UnixMilli(1<<48-1), ff)
	if id != "team_7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Fatalf("max id = %s", id)
	}
	// Known vector: time 1469918176385 ms encodes as 01ARYZ6S41.
	id, _ = domain.NewID(domain.PrefixTeam, time.UnixMilli(1469918176385), bytes.NewReader(make([]byte, 10)))
	if !strings.HasPrefix(string(id), "team_01ARYZ6S41") {
		t.Fatalf("time prefix = %s", id)
	}
	if _, err := domain.ParseID(domain.PrefixTeam, "team_8ZZZZZZZZZZZZZZZZZZZZZZZZZ"); err == nil {
		t.Error("first character above 7 overflows 128 bits")
	}
}
