package sqs

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// receiverActions are the actions only the owning pool's role may use on its inbox queue.
var receiverActions = []string{"sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:PurgeQueue", "sqs:GetQueueAttributes"}

// senderActions are the actions the pools with a handoff to the queue's pool may use.
var senderActions = []string{"sqs:SendMessage"}

type statement struct {
	Sid       string         `json:"Sid"`
	Effect    string         `json:"Effect"`
	Principal map[string]any `json:"Principal"`
	Action    []string       `json:"Action"`
	Resource  string         `json:"Resource"`
	Condition map[string]any `json:"Condition,omitempty"`
}

// InboxPolicy returns the resource policy of a pool's inbox queue: only the listed sender roles may send and
// only the receiver role may receive, delete, purge or change visibility. The allows name the account root and
// narrow with aws:PrincipalArn, so a role that does not exist yet does not make the policy invalid; the denies
// close the queue to every other principal, including others in the same account. The reconciler manages the
// queue through its own queue-administration permissions, which the denies do not cover. The output is
// deterministic (sorted senders) so an unchanged queue is not rewritten.
func InboxPolicy(queueARN, receiverRoleARN string, senderRoleARNs []string) (string, error) {
	account, err := accountOf(receiverRoleARN)
	if err != nil {
		return "", err
	}
	if q := strings.Split(queueARN, ":"); len(q) != 6 || q[2] != "sqs" {
		return "", errors.New("sqs: not an SQS queue ARN")
	}
	senders := slices.Clone(senderRoleARNs)
	slices.Sort(senders)
	senders = slices.Compact(senders)
	partition := strings.Split(receiverRoleARN, ":")[1]
	root := map[string]any{"AWS": fmt.Sprintf("arn:%s:iam::%s:root", partition, account)}
	everyone := map[string]any{"AWS": "*"}

	stmts := []statement{
		{Sid: "ReceiverAllow", Effect: "Allow", Principal: root, Action: receiverActions, Resource: queueARN,
			Condition: map[string]any{"ArnEquals": map[string]any{"aws:PrincipalArn": receiverRoleARN}}},
		{Sid: "ReceiverOnly", Effect: "Deny", Principal: everyone, Action: receiverActions, Resource: queueARN,
			Condition: map[string]any{"ArnNotEquals": map[string]any{"aws:PrincipalArn": receiverRoleARN}}},
	}
	if len(senders) > 0 {
		stmts = append(stmts,
			statement{Sid: "SendersAllow", Effect: "Allow", Principal: root, Action: senderActions, Resource: queueARN,
				Condition: map[string]any{"ArnEquals": map[string]any{"aws:PrincipalArn": senders}}},
			statement{Sid: "SendersOnly", Effect: "Deny", Principal: everyone, Action: senderActions, Resource: queueARN,
				Condition: map[string]any{"ArnNotEquals": map[string]any{"aws:PrincipalArn": senders}}})
	} else {
		stmts = append(stmts, statement{Sid: "NoSenders", Effect: "Deny", Principal: everyone, Action: senderActions, Resource: queueARN})
	}
	b, err := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": stmts})
	if err != nil {
		return "", fmt.Errorf("sqs: marshal policy: %w", err)
	}
	return string(b), nil
}

func accountOf(roleARN string) (string, error) {
	p := strings.Split(roleARN, ":")
	if len(p) < 6 || p[0] != "arn" || p[2] != "iam" || p[4] == "" {
		return "", errors.New("sqs: not an IAM role ARN")
	}
	return p[4], nil
}
