package team

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Tag keys on AWS resources (D25, D26): human names and ids are tags, never part of the name.
const (
	TagTeamID   = "afs-team-id"
	TagTeamSlug = "afs-team-slug"
	// TagManagedBy marks resources the reconciler created, so it only ever deletes its own.
	TagManagedBy = "afs-managed-by"
	// ManagedByValue is the value of TagManagedBy.
	ManagedByValue = "afs-reconciler"
)

// RoleNameForID returns the IAM role name of a resource: "afs-<ulid>" (D25). Two agents with the same
// human name in different teams have different ids, so their roles never collide.
func RoleNameForID(id domain.ID) (string, error) {
	_, ulid, ok := strings.Cut(string(id), "_")
	if !ok || ulid == "" {
		return "", fmt.Errorf("team: %q is not a prefixed id", id)
	}
	return "afs-" + ulid, nil
}

// TeamRoleName is the team's IAM role name.
func TeamRoleName(teamID string) (string, error) { return RoleNameForID(domain.ID(teamID)) }

// BedrockRoleName is the name of the role the gateway assumes for the team: "afs-<team ulid>-bedrock".
func BedrockRoleName(teamID string) (string, error) {
	n, err := TeamRoleName(teamID)
	if err != nil {
		return "", err
	}
	return n + "-bedrock", nil
}

// RolePath is the IAM path of every role of a namespace: "/afs/<namespace>/".
func RolePath(namespace string) string { return "/afs/" + namespace + "/" }

var profileCharsRE = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// ProfileName is the inference profile name for a team model: "afs-<team ulid>-<model id>" with every
// character Bedrock does not allow replaced by "-", at most 64 characters. Distinct models of one team
// keep distinct names unless they differ only in punctuation; a hash-free suffix is kept short on
// purpose so the name stays readable in the console.
func ProfileName(teamID, modelID string) (string, error) {
	n, err := TeamRoleName(teamID)
	if err != nil {
		return "", err
	}
	name := n + "-" + profileCharsRE.ReplaceAllString(modelID, "-")
	if len(name) > 64 {
		name = name[:64]
	}
	return strings.TrimRight(name, "-_"), nil
}

// RoleARN builds the ARN of a role from its parts.
func RoleARN(partition, accountID, path, name string) string {
	return fmt.Sprintf("arn:%s:iam::%s:role%s%s", partition, accountID, path, name)
}

// SourceARN returns the ARN a team model's inference profile copies from. A model id that is already
// an ARN is used as is; an id with a geography prefix (us., eu., apac., global.) names a system-defined
// cross-region inference profile in the account; any other id is a foundation model.
func SourceARN(partition, region, accountID, modelID string) string {
	switch {
	case strings.HasPrefix(modelID, "arn:"):
		return modelID
	case hasGeoPrefix(modelID):
		return fmt.Sprintf("arn:%s:bedrock:%s:%s:inference-profile/%s", partition, region, accountID, modelID)
	}
	return fmt.Sprintf("arn:%s:bedrock:%s::foundation-model/%s", partition, region, modelID)
}

func hasGeoPrefix(id string) bool {
	for _, p := range []string{"us.", "eu.", "apac.", "global.", "us-gov.", "jp.", "au.", "ca."} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}
