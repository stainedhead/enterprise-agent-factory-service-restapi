package domain

import (
	"strings"
	"testing"
	"time"
)

func TestNewModel(t *testing.T) {
	good := NewModelParams{ID: "anthropic.claude-sonnet-4-5", Provider: "anthropic", DisplayName: "Sonnet", ContextWindow: 200000, MaxOutputTokens: 8192}
	tests := []struct {
		name    string
		mod     func(*NewModelParams)
		wantErr string
	}{
		{"ok", func(*NewModelParams) {}, ""},
		{"disabled", func(p *NewModelParams) { p.Enabled = ptr(false) }, ""},
		{"max output required (D28)", func(p *NewModelParams) { p.MaxOutputTokens = 0 }, "maxOutputTokens"},
		{"max output above window", func(p *NewModelParams) { p.MaxOutputTokens = 300000 }, "maxOutputTokens"},
		{"window required", func(p *NewModelParams) { p.ContextWindow = 0 }, "contextWindow"},
		{"version suffix with a colon", func(p *NewModelParams) { p.ID = "anthropic.claude-v2:0" }, ""},
		{"space refused", func(p *NewModelParams) { p.ID = "a b" }, "id"},
		{"empty id", func(p *NewModelParams) { p.ID = "" }, "id"},
		{"empty provider", func(p *NewModelParams) { p.Provider = " " }, "provider"},
		{"long name", func(p *NewModelParams) { p.DisplayName = strings.Repeat("x", 201) }, "displayName"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := good
			tc.mod(&p)
			m, err := NewModel(p)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if want := p.Enabled == nil || *p.Enabled; m.Enabled != want {
					t.Errorf("enabled = %v", m.Enabled)
				}
				return
			}
			if KindOf(err) != KindValidation || !strings.Contains(strings.Join(fieldsOf(err), ","), tc.wantErr) {
				t.Errorf("err = %v, want a ValidationError on %s", err, tc.wantErr)
			}
		})
	}
}

func TestModelApply(t *testing.T) {
	base := Model{ID: "m", Provider: "p", DisplayName: "M", Enabled: true, ContextWindow: 1000, MaxOutputTokens: 500}
	tests := []struct {
		name    string
		u       ModelUpdate
		wantErr bool
		check   func(Model) bool
	}{
		{"disable", ModelUpdate{Enabled: ptr(false)}, false, func(m Model) bool { return !m.Enabled }},
		{"rename", ModelUpdate{DisplayName: ptr("New")}, false, func(m Model) bool { return m.DisplayName == "New" }},
		{"empty patch", ModelUpdate{}, true, nil},
		{"zero window", ModelUpdate{ContextWindow: ptr(0)}, true, nil},
		{"output above window", ModelUpdate{MaxOutputTokens: ptr(2000)}, true, nil},
		{"window below output", ModelUpdate{ContextWindow: ptr(100)}, true, nil},
		{"raise both", ModelUpdate{ContextWindow: ptr(4000), MaxOutputTokens: ptr(2000)}, false, func(m Model) bool { return m.MaxOutputTokens == 2000 }},
		{"blank name", ModelUpdate{DisplayName: ptr("")}, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := base.Apply(tc.u)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err != nil && KindOf(err) != KindValidation {
				t.Errorf("kind = %v", KindOf(err))
			}
			if err == nil && !tc.check(m) {
				t.Errorf("model = %+v", m)
			}
		})
	}
}

func TestNewModelPrice(t *testing.T) {
	at := time.Date(2026, 12, 1, 0, 0, 0, 0, time.FixedZone("x", 3600))
	good := NewModelPriceParams{EffectiveFrom: at, Input: MustMoney("3.00"), Output: MustMoney("15.00")}
	p, err := NewModelPrice("m1", good)
	if err != nil || p.Source != PriceSourceManual || !p.EffectiveFrom.Equal(at) || p.EffectiveFrom.Location() != time.UTC || p.CacheRead.Micros() != 0 {
		t.Fatalf("price = %+v, %v", p, err)
	}
	c := MustMoney("0.30")
	good.CacheRead, good.CacheWrite = &c, &c
	if p, err = NewModelPrice("m1", good); err != nil || p.CacheWrite != c {
		t.Fatalf("cache prices = %+v, %v", p, err)
	}
	for name, mod := range map[string]func(*NewModelPriceParams){
		"no time":        func(p *NewModelPriceParams) { p.EffectiveFrom = time.Time{} },
		"negative input": func(p *NewModelPriceParams) { p.Input = MoneyFromMicros(-1) },
		"huge output":    func(p *NewModelPriceParams) { p.Output = MustMoney("1000000") },
		"negative cache": func(p *NewModelPriceParams) { n := MoneyFromMicros(-5); p.CacheRead = &n },
		"huge cache w":   func(p *NewModelPriceParams) { n := MustMoney("1000000"); p.CacheWrite = &n },
	} {
		q := NewModelPriceParams{EffectiveFrom: at, Input: MustMoney("1"), Output: MustMoney("1")}
		mod(&q)
		if _, err := NewModelPrice("m1", q); KindOf(err) != KindValidation {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
