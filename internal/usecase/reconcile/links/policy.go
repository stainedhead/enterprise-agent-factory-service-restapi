package links

import (
	"encoding/json"
	"fmt"
	"sort"
)

// SidAssumeLinks and SidVerifier name the statements this package adds to an agent role's policies.
const (
	SidAssumeLinks = "AssumeLinkedRoles"
	SidVerifier    = "LinkVerification"
)

// WithAssumeStatements adds to an IAM policy document one statement that allows sts:AssumeRole on the
// exact ARN of each link, in sorted order and without duplicates (RC-US-4.1). With no ARN the document
// is returned unchanged, so an agent without links gets no statement. No wildcard is ever used.
func WithAssumeStatements(policy string, roleARNs []string) (string, error) {
	arns := uniqueSorted(roleARNs)
	if len(arns) == 0 {
		return policy, nil
	}
	return addStatement(policy, map[string]any{
		"Sid": SidAssumeLinks, "Effect": "Allow", "Action": []string{"sts:AssumeRole"}, "Resource": arns,
	})
}

// TrustWithVerifier adds to a trust policy the reconciler's role as a principal that may assume the
// agent role to verify its links. With no verifier role the document is returned unchanged. The
// statement is limited to the one principal and names no wildcard.
func TrustWithVerifier(trust, verifierRoleARN string) (string, error) {
	if verifierRoleARN == "" {
		return trust, nil
	}
	return addStatement(trust, map[string]any{
		"Sid": SidVerifier, "Effect": "Allow", "Principal": map[string]any{"AWS": verifierRoleARN},
		"Action": []string{"sts:AssumeRole", "sts:TagSession"},
	})
}

func addStatement(doc string, st map[string]any) (string, error) {
	var p struct {
		Version   string           `json:"Version"`
		Statement []map[string]any `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return "", fmt.Errorf("links: policy is not JSON: %w", err)
	}
	for _, s := range p.Statement {
		if s["Sid"] == st["Sid"] {
			return "", fmt.Errorf("links: policy already has statement %v", st["Sid"])
		}
	}
	p.Statement = append(p.Statement, st)
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
