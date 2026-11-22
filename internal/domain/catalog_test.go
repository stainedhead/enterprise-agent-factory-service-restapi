package domain

import (
	"strings"
	"testing"
)

func TestNewStorageClass(t *testing.T) {
	tests := []struct {
		name    string
		sc      string
		vt      VolumeType
		desc    string
		enabled *bool
		bad     string
	}{
		{"gp3", "gp3", VolumeTypeGP3, "", nil, ""},
		{"io2 off", "fast-io2", VolumeTypeIO2, "d", ptr(false), ""},
		{"upper case", "GP3", VolumeTypeGP3, "", nil, "name"},
		{"digit first", "1gp", VolumeTypeGP3, "", nil, "name"},
		{"efs refused", "shared", VolumeType("efs"), "", nil, "volumeType"},
		{"long description", "gp3", VolumeTypeGP3, strings.Repeat("x", 501), nil, "description"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc, err := NewStorageClass(tc.sc, tc.desc, tc.vt, tc.enabled)
			if tc.bad == "" {
				if err != nil || sc.Enabled != (tc.enabled == nil || *tc.enabled) {
					t.Fatalf("sc = %+v, %v", sc, err)
				}
				return
			}
			if KindOf(err) != KindValidation || fieldsOf(err)[0] != tc.bad {
				t.Errorf("err = %v", err)
			}
		})
	}
}

const (
	dig1 = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dig2 = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestNewAgentTemplate(t *testing.T) {
	good := PutAgentTemplateParams{Name: "hermes-std", Harness: "hermes", AllowedDigests: []string{dig1, dig2}, CurrentDigest: dig2,
		DefaultResources: Resources{CPU: "500m", Memory: "2Gi"}}
	tests := []struct {
		name string
		mod  func(*PutAgentTemplateParams)
		bad  string
	}{
		{"ok", func(*PutAgentTemplateParams) {}, ""},
		{"disabled", func(p *PutAgentTemplateParams) { p.Enabled = ptr(false) }, ""},
		{"bad name", func(p *PutAgentTemplateParams) { p.Name = "Hermes" }, "name"},
		{"bad harness", func(p *PutAgentTemplateParams) { p.Harness = "-x" }, "harness"},
		{"no digests", func(p *PutAgentTemplateParams) { p.AllowedDigests = nil }, "allowedDigests"},
		{"malformed digest", func(p *PutAgentTemplateParams) { p.AllowedDigests = []string{"sha256:abc"} }, "allowedDigests"},
		{"repeated digest", func(p *PutAgentTemplateParams) { p.AllowedDigests = []string{dig1, dig1} }, "allowedDigests"},
		{"current not allowed", func(p *PutAgentTemplateParams) { p.CurrentDigest = "sha256:" + strings.Repeat("c", 64) }, "currentDigest"},
		{"current malformed", func(p *PutAgentTemplateParams) { p.CurrentDigest = "x" }, "currentDigest"},
		{"bad cpu", func(p *PutAgentTemplateParams) { p.DefaultResources.CPU = "lots" }, "defaultResources.cpu"},
		{"bad memory", func(p *PutAgentTemplateParams) { p.DefaultResources.Memory = "2 GB" }, "defaultResources.memory"},
		{"long description", func(p *PutAgentTemplateParams) { p.Description = strings.Repeat("x", 1001) }, "description"},
		{"too many digests", func(p *PutAgentTemplateParams) { p.AllowedDigests = make([]string, 51) }, "allowedDigests"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := good
			tc.mod(&p)
			tpl, err := NewAgentTemplate(p)
			if tc.bad == "" {
				if err != nil {
					t.Fatal(err)
				}
				if tpl.Enabled != (p.Enabled == nil || *p.Enabled) || !tpl.AllowsDigest(dig1) || tpl.AllowsDigest("sha256:x") {
					t.Errorf("template = %+v", tpl)
				}
				return
			}
			if KindOf(err) != KindValidation || !strings.Contains(strings.Join(fieldsOf(err), ","), tc.bad) {
				t.Errorf("err = %v, want %s", err, tc.bad)
			}
		})
	}
}
