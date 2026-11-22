package pool

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Statement ids this package adds to the pool role's policy.
const (
	SidReceiveInbox = "ReceiveInbox"
	SidSendHandoffs = "SendHandoffs"
)

// WithQueueStatements adds to the pool role's policy document the allowance to use its queues: receive,
// delete and change visibility on its own inbox, and send on the inbox of each pool it hands work to. Each
// statement names exact queue ARNs, never a wildcard, and is left out when it would name none. The queue's
// own policy is the other half: it names the same roles and denies everyone else (adapter/aws/sqs).
func WithQueueStatements(policy, inboxARN string, handoffARNs []string) (string, error) {
	var p struct {
		Version   string           `json:"Version"`
		Statement []map[string]any `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(policy), &p); err != nil {
		return "", fmt.Errorf("pool: policy is not JSON: %w", err)
	}
	if inboxARN != "" {
		p.Statement = append(p.Statement, map[string]any{
			"Sid": SidReceiveInbox, "Effect": "Allow",
			"Action":   []string{"sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"},
			"Resource": []string{inboxARN},
		})
	}
	if arns := uniqueSorted(handoffARNs); len(arns) > 0 {
		p.Statement = append(p.Statement, map[string]any{
			"Sid": SidSendHandoffs, "Effect": "Allow", "Action": []string{"sqs:SendMessage", "sqs:GetQueueAttributes"}, "Resource": arns,
		})
	}
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
