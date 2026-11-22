package secretsmanager

import (
	"context"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// Managed is a secret under the AFS path prefix, as the orphan sweep sees it.
type Managed struct {
	ARN     string
	Name    string
	Created time.Time
	// Orphan is true when the secret is tagged afs-orphan=true.
	Orphan bool
}

// ListUnderPrefix returns every secret whose name starts with prefix + "/" that is not already
// scheduled for deletion, following pagination.
func (c *Client) ListUnderPrefix(ctx context.Context, prefix string) ([]Managed, error) {
	in := &sm.ListSecretsInput{Filters: []types.Filter{{Key: types.FilterNameStringTypeName, Values: []string{prefix + "/"}}}}
	var out []Managed
	for {
		page, err := c.api.ListSecrets(ctx, in)
		if err != nil {
			return nil, wrap(err)
		}
		for _, s := range page.SecretList {
			name := aws.ToString(s.Name)
			if !strings.HasPrefix(name, prefix+"/") || s.DeletedDate != nil {
				continue
			}
			m := Managed{ARN: aws.ToString(s.ARN), Name: name, Orphan: hasTag(s.Tags, OrphanTagKey, "true")}
			if s.CreatedDate != nil {
				m.Created = *s.CreatedDate
			}
			out = append(out, m)
		}
		if page.NextToken == nil || *page.NextToken == "" {
			return out, nil
		}
		in.NextToken = page.NextToken
	}
}
