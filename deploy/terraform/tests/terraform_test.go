// Package tests checks the Terraform configuration without a terraform binary: every file parses
// as HCL, no identifier is hardcoded, and the security properties of the data, backup and IAM
// modules hold. `terraform validate` and `tflint` run in CI (.github/workflows/deploy.yml) on top
// of this. It is a separate Go module so the HCL parser stays out of the service module; run it
// with `make deploy-test`.
package tests

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

const root = ".."

// parsed maps a path relative to root to its syntax body.
func parsed(t *testing.T) map[string]*hclsyntax.Body {
	t.Helper()
	out := map[string]*hclsyntax.Body{}
	p := hclparse.NewParser()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "tests" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".tf") {
			return nil
		}
		f, diags := p.ParseHCLFile(path)
		if diags.HasErrors() {
			t.Errorf("%s: %v", path, diags)
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = f.Body.(*hclsyntax.Body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 15 {
		t.Fatalf("only %d .tf files found", len(out))
	}
	return out
}

func TestEveryFileParses(t *testing.T) { parsed(t) }

func TestNoHardcodedIdentifiers(t *testing.T) {
	account := regexp.MustCompile(`\b\d{12}\b`)
	region := regexp.MustCompile(`"(us|eu|ap|sa|ca|me|af)-[a-z]+-\d"`)
	arn := regexp.MustCompile(`arn:aws[a-z-]*:[a-z0-9-]+:[a-z0-9-]*:\d*:`)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, _ error) error {
		if d.IsDir() || !strings.HasSuffix(path, ".tf") {
			return nil
		}
		b, _ := os.ReadFile(path)
		s := string(b)
		if account.MatchString(s) {
			t.Errorf("%s hardcodes an account id", path)
		}
		if region.MatchString(s) {
			t.Errorf("%s hardcodes a region", path)
		}
		for _, m := range arn.FindAllString(s, -1) {
			if !strings.Contains(m, "::aws:") && !strings.Contains(m, "${") {
				t.Errorf("%s hardcodes the ARN prefix %q", path, m)
			}
		}
		return nil
	})
}

func blocks(b *hclsyntax.Body, typ string, labels ...string) []*hclsyntax.Block {
	var out []*hclsyntax.Block
	for _, bl := range b.Blocks {
		if bl.Type != typ || len(bl.Labels) < len(labels) {
			continue
		}
		ok := true
		for i, l := range labels {
			if l != "*" && bl.Labels[i] != l {
				ok = false
			}
		}
		if ok {
			out = append(out, bl)
		}
	}
	return out
}

func attrText(src map[string][]byte, file string, a *hclsyntax.Attribute) string {
	r := a.Expr.Range()
	return string(src[file][r.Start.Byte:r.End.Byte])
}

func sources(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, _ error) error {
		if !d.IsDir() && strings.HasSuffix(path, ".tf") {
			rel, _ := filepath.Rel(root, path)
			b, _ := os.ReadFile(path)
			out[rel] = b
		}
		return nil
	})
	return out
}

func attr(t *testing.T, src map[string][]byte, b *hclsyntax.Body, file, name string) string {
	t.Helper()
	a, ok := b.Attributes[name]
	if !ok {
		t.Fatalf("%s: attribute %s missing", file, name)
	}
	return attrText(src, file, a)
}

func TestDatabaseIsEncryptedPrivateAndRecoverable(t *testing.T) {
	files, src := parsed(t), sources(t)
	f := "modules/rds/main.tf"
	dbs := blocks(files[f], "resource", "aws_db_instance")
	if len(dbs) != 1 {
		t.Fatalf("%d aws_db_instance resources", len(dbs))
	}
	b := dbs[0].Body
	want := map[string]string{
		"engine":                              `"postgres"`,
		"engine_version":                      `"17"`,
		"storage_encrypted":                   "true",
		"iam_database_authentication_enabled": "true",
		"publicly_accessible":                 "false",
		"manage_master_user_password":         "true",
		"deletion_protection":                 "true",
		"backup_retention_period":             "var.backup_retention_days",
		"delete_automated_backups":            "false",
	}
	for k, v := range want {
		if got := attr(t, src, b, f, k); got != v {
			t.Errorf("aws_db_instance.%s = %s, want %s", k, got, v)
		}
	}
	for _, forbidden := range []string{"password", "master_password"} {
		if _, ok := b.Attributes[forbidden]; ok {
			t.Errorf("aws_db_instance sets %s: the master password must be RDS-managed", forbidden)
		}
	}
	if attr(t, src, b, f, "kms_key_id") != "aws_kms_key.db.arn" {
		t.Error("storage is not encrypted with the module's customer-managed key")
	}
	// TLS is enforced and the retention variable is bounded to what PITR allows.
	if !strings.Contains(string(src[f]), `"rds.force_ssl"`) {
		t.Error("rds.force_ssl not set")
	}
	v := string(src["modules/rds/variables.tf"])
	if !strings.Contains(v, "var.backup_retention_days >= 1") || !strings.Contains(v, "<= 35") {
		t.Error("backup_retention_days is not validated to 1..35")
	}
}

func TestBackupCopiesCrossRegionAndCrossAccount(t *testing.T) {
	files, src := parsed(t), sources(t)
	f := "modules/backup/main.tf"
	plans := blocks(files[f], "resource", "aws_backup_plan")
	if len(plans) != 1 {
		t.Fatal("want one backup plan")
	}
	rule := blocks(plans[0].Body, "rule")[0].Body
	var dests []string
	for _, c := range blocks(rule, "copy_action") {
		dests = append(dests, attr(t, src, c.Body, f, "destination_vault_arn"))
	}
	sort.Strings(dests)
	if len(dests) != 2 || dests[0] != "aws_backup_vault.dr.arn" || dests[1] != "var.cross_account_backup_vault_arn" {
		t.Errorf("copy destinations = %v, want the DR-region vault and the cross-account vault", dests)
	}
	for _, v := range blocks(files[f], "resource", "aws_backup_vault") {
		if _, ok := v.Body.Attributes["kms_key_arn"]; !ok {
			t.Errorf("vault %s is not encrypted with a customer-managed key", v.Labels[1])
		}
	}
	// The DR vault and key must live in the DR region.
	for _, r := range append(blocks(files[f], "resource", "aws_backup_vault", "dr"), blocks(files[f], "resource", "aws_kms_key", "dr_vault")...) {
		if attr(t, src, r.Body, f, "provider") != "aws.dr" {
			t.Errorf("%s.%s does not use the DR provider", r.Labels[0], r.Labels[1])
		}
	}
	// Selection covers the resources handed in.
	if len(blocks(files[f], "resource", "aws_backup_selection")) != 1 {
		t.Error("no backup selection")
	}
}

func TestBucketsAreHardened(t *testing.T) {
	files, src := parsed(t), sources(t)
	f := "modules/buckets/main.tf"
	for _, typ := range []string{"aws_s3_bucket_public_access_block", "aws_s3_bucket_versioning", "aws_s3_bucket_server_side_encryption_configuration", "aws_s3_bucket_lifecycle_configuration", "aws_s3_bucket_policy"} {
		bs := blocks(files[f], "resource", typ)
		if len(bs) != 1 {
			t.Errorf("%s: %d resources, want 1 (for_each over both buckets)", typ, len(bs))
			continue
		}
		if attr(t, src, bs[0].Body, f, "for_each") != "aws_s3_bucket.this" {
			t.Errorf("%s does not cover every bucket", typ)
		}
	}
	pab := blocks(files[f], "resource", "aws_s3_bucket_public_access_block")[0].Body
	for _, k := range []string{"block_public_acls", "block_public_policy", "ignore_public_acls", "restrict_public_buckets"} {
		if attr(t, src, pab, f, k) != "true" {
			t.Errorf("public access block %s is not true", k)
		}
	}
	text := string(src[f])
	for _, must := range []string{`sse_algorithm     = "aws:kms"`, "aws:SecureTransport", "s3:x-amz-server-side-encryption"} {
		if !strings.Contains(text, must) {
			t.Errorf("buckets module lacks %q", must)
		}
	}
	if !regexp.MustCompile(`\bexport\s*=\s*\{`).MatchString(text) || !regexp.MustCompile(`\breports\s*=\s*\{`).MatchString(text) {
		t.Error("export and reports buckets are not both defined")
	}
}

// statements returns the evaluated literal actions and the sid of every IAM policy statement in a file.
type stmt struct {
	sid     string
	effect  string
	actions []string
	hasCond bool
}

func statements(t *testing.T, b *hclsyntax.Body, file string) []stmt {
	t.Helper()
	var out []stmt
	for _, doc := range blocks(b, "data", "aws_iam_policy_document") {
		for _, s := range blocks(doc.Body, "statement") {
			st := stmt{effect: "Allow", hasCond: len(blocks(s.Body, "condition")) > 0}
			for name, a := range s.Body.Attributes {
				v, diags := a.Expr.Value(&hcl.EvalContext{})
				if diags.HasErrors() {
					continue // references a variable: only literals matter here
				}
				switch name {
				case "sid":
					st.sid = v.AsString()
				case "effect":
					st.effect = v.AsString()
				case "actions":
					for _, e := range v.AsValueSlice() {
						if e.Type() == cty.String {
							st.actions = append(st.actions, e.AsString())
						}
					}
				}
			}
			if st.sid == "" {
				t.Errorf("%s: a policy statement has no sid", file)
			}
			out = append(out, st)
		}
	}
	return out
}

func TestIAMLeastPrivilege(t *testing.T) {
	files := parsed(t)
	f := "modules/pod-identity/main.tf"
	sts := statements(t, files[f], f)
	if len(sts) < 15 {
		t.Fatalf("only %d statements evaluated", len(sts))
	}
	createRole := 0
	for _, s := range sts {
		for _, a := range s.actions {
			switch {
			case a == "*" || a == "*:*":
				t.Errorf("statement %s grants every action", s.sid)
			case strings.HasPrefix(a, "bedrock:Invoke") || strings.HasPrefix(a, "bedrock:Converse"):
				t.Errorf("statement %s lets a control-plane role invoke models: only a team's Bedrock role may", s.sid)
			case a == "bedrock:*" && s.effect == "Allow":
				t.Errorf("statement %s allows bedrock:*", s.sid)
			case a == "iam:*" || a == "s3:*" || a == "secretsmanager:*" || a == "sts:*":
				if s.effect == "Allow" {
					t.Errorf("statement %s allows %s", s.sid, a)
				}
			case a == "iam:CreateRole":
				createRole++
				if !s.hasCond {
					t.Errorf("statement %s grants iam:CreateRole without the permissions-boundary condition", s.sid)
				}
			}
		}
	}
	if createRole != 1 {
		t.Errorf("iam:CreateRole appears in %d statements, want exactly one (the bounded one)", createRole)
	}
	byID := map[string]stmt{}
	for _, s := range sts {
		byID[s.sid] = s
	}
	// afs-api can write secret values but never read them back.
	if s := byID["DenyReadingSecretValues"]; s.effect != "Deny" || len(s.actions) == 0 {
		t.Error("afs-api is not denied GetSecretValue")
	}
	for _, a := range byID["WriteOnlySecretValues"].actions {
		if strings.Contains(a, "Get") {
			t.Errorf("afs-api may %s", a)
		}
	}
	// the gateway assumes roles and does nothing else
	g := byID["AssumeTeamBedrockRoles"]
	if len(g.actions) != 2 || g.actions[0] != "sts:AssumeRole" {
		t.Errorf("gateway actions = %v", g.actions)
	}
	// M4: the reconciler mirrors organization accounts and verifies role links, the latter only on /afs/ roles.
	if s := byID["OrganizationAccountMirror"]; len(s.actions) != 2 || s.actions[0] != "organizations:ListAccounts" {
		t.Errorf("organization mirror actions = %v", s.actions)
	}
	if s := byID["VerifyAgentRoleLinks"]; len(s.actions) != 2 || s.actions[0] != "sts:AssumeRole" || s.effect != "Allow" {
		t.Errorf("role link verification = %+v", s)
	}
	// trust is Pod Identity only
	src := sources(t)[f]
	if !strings.Contains(string(src), `role/afs/*"]`) {
		t.Error("role link verification is not limited to roles under /afs/")
	}
	if !strings.Contains(string(src), `identifiers = ["pods.eks.amazonaws.com"]`) {
		t.Error("roles are not trusted by Pod Identity")
	}
	for _, role := range []string{"api", "reconciler", "gateway", "billing"} {
		if !strings.Contains(string(src), `"`+role+`"`) {
			t.Errorf("role %s missing", role)
		}
	}
}

func TestBoundaryDeniesBedrockAndIAM(t *testing.T) {
	files := parsed(t)
	f := "modules/boundary/main.tf"
	denied := map[string]bool{}
	for _, s := range statements(t, files[f], f) {
		if s.effect == "Deny" {
			for _, a := range s.actions {
				denied[a] = true
			}
		}
	}
	for _, a := range []string{"bedrock:*", "iam:*", "organizations:*"} {
		if !denied[a] {
			t.Errorf("the permissions boundary does not deny %s", a)
		}
	}
}

func TestRootWiresTheModules(t *testing.T) {
	files := parsed(t)
	got := map[string]bool{}
	for _, m := range blocks(files["main.tf"], "module") {
		got[m.Labels[0]] = true
	}
	for _, want := range []string{"rds", "backup", "buckets", "boundary", "pod_identity"} {
		if !got[want] {
			t.Errorf("root does not instantiate module %s", want)
		}
	}
	outs := map[string]bool{}
	for _, o := range blocks(files["outputs.tf"], "output") {
		outs[o.Labels[0]] = true
	}
	for _, want := range []string{"database_endpoint", "export_bucket", "reports_bucket", "permissions_boundary_arn", "gateway_role_arn", "reconciler_role_arn", "buckets_kms_key_id"} {
		if !outs[want] {
			t.Errorf("output %s missing (the Helm chart and reconciler need it)", want)
		}
	}
}

func TestModuleVariablesAreDeclared(t *testing.T) {
	// Every var.<name> a module uses is declared in that module.
	files := parsed(t)
	src := sources(t)
	ref := regexp.MustCompile(`\bvar\.([a-z0-9_]+)`)
	declared := map[string]map[string]bool{}
	for f, b := range files {
		dir := filepath.Dir(f)
		if declared[dir] == nil {
			declared[dir] = map[string]bool{}
		}
		for _, v := range blocks(b, "variable") {
			declared[dir][v.Labels[0]] = true
		}
	}
	for f, s := range src {
		dir := filepath.Dir(f)
		for _, m := range ref.FindAllStringSubmatch(string(s), -1) {
			if !declared[dir][m[1]] {
				t.Errorf("%s uses var.%s, which %s does not declare", f, m[1], dir)
			}
		}
	}
}
