package main

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
)

type jobNames struct{ names []string }

func (j *jobNames) Every(name string, _ time.Duration, _ func(context.Context) error) {
	j.names = append(j.names, name)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// M6-E: the jobs that need no configuration are always registered; intake needs the queue URL.
func TestWireWorkOrdersRegistersTheJobs(t *testing.T) {
	var s jobNames
	if err := wireWorkOrders(&s, awssdk.Config{}, nil, nil, nil, nil, envOf(nil), time.Now, quiet()); err != nil {
		t.Fatal(err)
	}
	sort.Strings(s.names)
	if got := strings.Join(s.names, ","); got != "work-order-assign,work-order-notify,work-order-poll,work-order-settle" {
		t.Errorf("without configuration: %s", got)
	}

	s = jobNames{}
	env := envOf(map[string]string{envWorkOrderQueueURL: "https://sqs.us-east-1.amazonaws.com/111122223333/afs-work-orders", envNotifyEmailFrom: "afs@example.com"})
	if err := wireWorkOrders(&s, awssdk.Config{}, nil, nil, nil, nil, env, time.Now, quiet()); err != nil {
		t.Fatal(err)
	}
	sort.Strings(s.names)
	if got := strings.Join(s.names, ","); got != "work-order-assign,work-order-intake,work-order-notify,work-order-poll,work-order-settle" {
		t.Errorf("with the queue: %s", got)
	}
}

func TestWireWorkOrdersRefusesBadConfiguration(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"queue url": {envWorkOrderQueueURL: "afs-work-orders"},
		"apps file": {envGitHubAppsFile: "/nonexistent/apps.json"},
	} {
		var s jobNames
		if err := wireWorkOrders(&s, awssdk.Config{}, nil, nil, nil, nil, envOf(env), time.Now, quiet()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
