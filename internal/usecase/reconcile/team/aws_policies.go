package team

import (
	"encoding/json"
	"fmt"
	"sort"
)

// policyDoc is an IAM policy document.
type policyDoc struct {
	Version   string      `json:"Version"`
	Statement []statement `json:"Statement"`
}

type statement struct {
	Sid       string         `json:"Sid,omitempty"`
	Effect    string         `json:"Effect"`
	Principal map[string]any `json:"Principal,omitempty"`
	Action    []string       `json:"Action"`
	Resource  []string       `json:"Resource,omitempty"`
	Condition map[string]any `json:"Condition,omitempty"`
}

func (p policyDoc) json() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("team: marshal policy: %w", err)
	}
	return string(b), nil
}

const policyVersion = "2012-10-17"

// ConditionNamespace restricts a statement to callers whose EKS Pod Identity session carries
// kubernetes-namespace = namespace (D26, OQ-1). Agent roles use it (M2); it is exported here so every
// controller builds the condition the same way.
func ConditionNamespace(namespace string) map[string]any {
	return map[string]any{"StringEquals": map[string]any{"aws:PrincipalTag/kubernetes-namespace": namespace}}
}

// ConditionTeamTag restricts a statement to sessions of roles tagged afs-team-id = teamID. Role tags
// are a property of the role and are visible on every session of it, even through a role chain
// (D26), which a Pod Identity session tag is not guaranteed to be.
func ConditionTeamTag(teamID string) map[string]any {
	return map[string]any{"StringEquals": map[string]any{"aws:PrincipalTag/" + TagTeamID: teamID}}
}

// TeamRoleTrust is the trust policy of the team role: EKS Pod Identity (with session tags) and, when
// set, the External Secrets Operator's role, which assumes the team role from the SecretStore.
func TeamRoleTrust(accountID, clusterARN, esoRoleARN string) (string, error) {
	cond := map[string]any{}
	if accountID != "" {
		cond["StringEquals"] = map[string]any{"aws:SourceAccount": accountID}
	}
	if clusterARN != "" {
		cond["ArnEquals"] = map[string]any{"aws:SourceArn": clusterARN}
	}
	st := []statement{{
		Sid: "PodIdentity", Effect: "Allow",
		Principal: map[string]any{"Service": "pods.eks.amazonaws.com"},
		Action:    []string{"sts:AssumeRole", "sts:TagSession"},
	}}
	if len(cond) > 0 {
		st[0].Condition = cond
	}
	if esoRoleARN != "" {
		st = append(st, statement{
			Sid: "ExternalSecretsOperator", Effect: "Allow",
			Principal: map[string]any{"AWS": esoRoleARN},
			Action:    []string{"sts:AssumeRole", "sts:TagSession"},
		})
	}
	return policyDoc{Version: policyVersion, Statement: st}.json()
}

// TeamRolePolicy is the inline policy of the team role: read the team's managed secrets (the path
// "<prefix>/<namespace>/*") and nothing else. It denies bedrock:* explicitly: only the gateway's
// Bedrock role may invoke models (D57).
func TeamRolePolicy(secretsPrefix, namespace, teamID string) (string, error) {
	if secretsPrefix == "" || namespace == "" || teamID == "" {
		return "", fmt.Errorf("team: secrets prefix, namespace and team id are required")
	}
	return policyDoc{Version: policyVersion, Statement: []statement{
		{
			Sid: "ReadTeamSecrets", Effect: "Allow",
			Action:    []string{"secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"},
			Resource:  []string{"arn:*:secretsmanager:*:*:secret:" + secretsPrefix + "/" + namespace + "/*"},
			Condition: ConditionTeamTag(teamID),
		},
		{Sid: "NoBedrock", Effect: "Deny", Action: []string{"bedrock:*"}, Resource: []string{"*"}},
	}}.json()
}

// CollaborationRolePolicy is the extra inline policy of the collaboration agent's own role (RC-28, SEC-7,
// COL-3): Paperclip manages its own secrets below "<prefix>/<namespace>/<agent name>/paperclip/" and writes
// its board key to the one exact name "<prefix>/<namespace>/<agent name>/paperclip-board-api-key". Nothing
// outside the agent's own path is reachable, and bedrock stays denied by the agent policy. kmsKeyARN is the
// environment key; without it there is no kms statement.
func CollaborationRolePolicy(secretsPrefix, namespace, agentName, teamID, kmsKeyARN string) (string, error) {
	if secretsPrefix == "" || namespace == "" || agentName == "" || teamID == "" {
		return "", fmt.Errorf("team: secrets prefix, namespace, agent name and team id are required")
	}
	base := secretsPrefix + "/" + namespace + "/" + agentName
	st := []statement{
		{
			Sid: "PaperclipOwnSecrets", Effect: "Allow",
			Action: []string{"secretsmanager:CreateSecret", "secretsmanager:PutSecretValue", "secretsmanager:UpdateSecret", "secretsmanager:DeleteSecret",
				"secretsmanager:TagResource", "secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"},
			Resource:  []string{"arn:*:secretsmanager:*:*:secret:" + base + "/paperclip/*"},
			Condition: ConditionTeamTag(teamID),
		},
		{
			// The six characters are the random suffix Secrets Manager appends to every secret ARN.
			Sid: "PaperclipBoardKey", Effect: "Allow",
			Action:    []string{"secretsmanager:PutSecretValue"},
			Resource:  []string{"arn:*:secretsmanager:*:*:secret:" + base + "/paperclip-board-api-key-??????"},
			Condition: ConditionTeamTag(teamID),
		},
	}
	if kmsKeyARN != "" {
		st = append(st, statement{
			Sid: "PaperclipSecretsKey", Effect: "Allow",
			Action:   []string{"kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey"},
			Resource: []string{kmsKeyARN},
			Condition: map[string]any{"StringLike": map[string]any{"kms:ViaService": "secretsmanager.*.amazonaws.com"},
				"StringEquals": map[string]any{"aws:PrincipalTag/" + TagTeamID: teamID}},
		})
	}
	return policyDoc{Version: policyVersion, Statement: st}.json()
}

// BedrockRoleTrust is the trust policy of the team's Bedrock role: only the gateway's role.
func BedrockRoleTrust(gatewayRoleARN string) (string, error) {
	if gatewayRoleARN == "" {
		return "", fmt.Errorf("team: the gateway role ARN is required")
	}
	return policyDoc{Version: policyVersion, Statement: []statement{{
		Sid: "GatewayOnly", Effect: "Allow",
		Principal: map[string]any{"AWS": gatewayRoleARN},
		Action:    []string{"sts:AssumeRole", "sts:TagSession"},
	}}}.json()
}

// ProfileGrant names one inference profile of the team and the foundation models behind it.
type ProfileGrant struct {
	ProfileARN string
	// ModelARN is the foundation model the profile routes to (a model ARN, region wildcard allowed).
	ModelARN string
}

// bedrockInvokeActions are the only Bedrock actions the Bedrock role receives. Converse needs
// InvokeModel and ConverseStream needs InvokeModelWithResponseStream, so these two cover all four
// runtime operations.
var bedrockInvokeActions = []string{"bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"}

// BedrockRolePolicy allows invoking the team's inference profiles and nothing else: the profiles
// themselves, and the underlying foundation models only when reached through one of those profiles
// (condition bedrock:InferenceProfileArn). With no profiles it denies every invocation.
func BedrockRolePolicy(grants []ProfileGrant) (string, error) {
	if len(grants) == 0 {
		return policyDoc{Version: policyVersion, Statement: []statement{{
			Sid: "NoModelsEnabled", Effect: "Deny", Action: bedrockInvokeActions, Resource: []string{"*"},
		}}}.json()
	}
	profiles, models := set(), set()
	for _, g := range grants {
		if g.ProfileARN == "" {
			return "", fmt.Errorf("team: a grant without a profile ARN")
		}
		profiles[g.ProfileARN] = true
		if g.ModelARN != "" {
			models[g.ModelARN] = true
		}
	}
	st := []statement{{
		Sid: "InvokeTeamProfiles", Effect: "Allow", Action: bedrockInvokeActions, Resource: sorted(profiles),
	}}
	if len(models) > 0 {
		st = append(st, statement{
			Sid: "InvokeBehindTeamProfiles", Effect: "Allow", Action: bedrockInvokeActions, Resource: sorted(models),
			Condition: map[string]any{"StringLike": map[string]any{"bedrock:InferenceProfileArn": sorted(profiles)}},
		})
	}
	return policyDoc{Version: policyVersion, Statement: st}.json()
}

func set() map[string]bool { return map[string]bool{} }

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
