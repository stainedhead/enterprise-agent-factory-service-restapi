package main

import (
	"log/slog"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/eventbridge"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/secretsmanager"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/ses"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sqs"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/notifyhttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

// Environment of notification delivery (M3-G3).
const (
	// envNotifyTargetRole is the role EventBridge assumes to invoke API destinations and bus targets.
	// Without it notification delivery is off.
	envNotifyTargetRole = "AFS_NOTIFY_TARGET_ROLE_ARN"
	// envNotifyEmailQueue is the ARN of the FIFO queue email channels target (content-based deduplication on).
	envNotifyEmailQueue = "AFS_NOTIFY_EMAIL_QUEUE_ARN"
	// envNotifyDLQ is the ARN of the dead-letter queue of the rule targets.
	envNotifyDLQ = "AFS_NOTIFY_DLQ_ARN"
	// envNotifyEmailFrom is the verified SES sender address.
	envNotifyEmailFrom = "AFS_NOTIFY_EMAIL_FROM"
)

// Periods of the notification jobs. The test poll is short because TestNotificationChannel waits 10 s.
const (
	notifyTestEvery    = 2 * time.Second
	notifyEmailEvery   = 5 * time.Second
	notifyFailureEvery = 15 * time.Second
	notifySweepEvery   = 30 * time.Second
)

// notifyWiring is the wiring of RC-15: the controller (nil when off), its route, a lister wrapper and
// the periodic jobs.
type notifyWiring struct {
	Controller *notify.Controller
	Routes     []engine.Route
	WrapLister func(engine.Lister) engine.Lister
	Register   func(*engine.Engine)
}

// notifyDeps are what wireNotify builds on.
type notifyDeps struct {
	AWS    awssdk.Config
	Bus    string
	Runner *postgres.TxRunner
	Status engine.StatusWriter
	Outbox port.OutboxWriter
	IDs    port.IDGen
	Pool   postgres.Querier2
	Env    func(string) string
	Now    func() time.Time
	Log    *slog.Logger
}

// wireNotify builds the notification controller and jobs. It returns an inert notifyWiring, with a
// warning, when AFS_NOTIFY_TARGET_ROLE_ARN is unset.
func wireNotify(d notifyDeps) (notifyWiring, error) {
	inert := notifyWiring{WrapLister: func(l engine.Lister) engine.Lister { return l }, Register: func(*engine.Engine) {}}
	role := d.Env(envNotifyTargetRole)
	if role == "" {
		d.Log.Warn("notification delivery is off: " + envNotifyTargetRole + " is not set")
		return inert, nil
	}
	rules := eventbridge.NewRules(d.AWS, eventbridge.RulesConfig{Bus: d.Bus, TargetRoleARN: role,
		EmailQueueARN: d.Env(envNotifyEmailQueue), DeadLetterARN: d.Env(envNotifyDLQ)})
	source := postgres.NewChannelSource(d.Runner)
	cstatus := postgres.NewChannelStatus(d.Runner, d.Status, d.Now)
	secrets := secretsmanager.NewReader(d.AWS)
	ctl, err := notify.NewController(notify.Config{Source: source, Bus: rules, Secrets: secrets, Cleaner: cstatus, Now: d.Now})
	if err != nil {
		return notifyWiring{}, err
	}
	events := postgres.NewChannelEvents(d.Runner, d.Outbox, d.IDs, d.Now)
	mailer := ses.New(d.AWS, d.Env(envNotifyEmailFrom))
	tests := &notify.TestRunner{
		Requests: postgres.NewChannelTests(d.Runner), Source: source, Secrets: secrets,
		Deliverer: notify.Deliverer{HTTP: notifyhttp.New(notifyhttp.Options{}), Mail: mailer, Bus: rules},
		Recorder:  cstatus, Events: events, Now: d.Now, Log: d.Log,
	}
	sweep := &notify.Sweeper{Bus: rules, Source: source, Cleaner: cstatus, Log: d.Log}
	var email *notify.EmailDrainer
	var failures *notify.FailureDrainer
	if arn := d.Env(envNotifyEmailQueue); arn != "" {
		q, err := sqs.New(d.AWS, arn)
		if err != nil {
			return notifyWiring{}, err
		}
		email = &notify.EmailDrainer{Queue: q, Source: source, Mailer: mailer, Recorder: cstatus, Events: events, Now: d.Now, Log: d.Log}
	} else {
		d.Log.Warn("email channels cannot be delivered: " + envNotifyEmailQueue + " is not set")
	}
	if arn := d.Env(envNotifyDLQ); arn != "" {
		q, err := sqs.New(d.AWS, arn)
		if err != nil {
			return notifyWiring{}, err
		}
		failures = &notify.FailureDrainer{Queue: q, Source: source, Recorder: cstatus, Events: events, Now: d.Now, Log: d.Log}
	} else {
		d.Log.Warn("delivery failures are not collected: " + envNotifyDLQ + " is not set")
	}
	return notifyWiring{
		Controller: ctl, Routes: []engine.Route{notify.Route()},
		WrapLister: func(l engine.Lister) engine.Lister { return postgres.NewChannelLister(l, d.Pool) },
		Register: func(e *engine.Engine) {
			e.Every("notify-tests", notifyTestEvery, tests.Run)
			e.Every("notify-sweep", notifySweepEvery, sweep.Run)
			if email != nil {
				e.Every("notify-email", notifyEmailEvery, email.Run)
			}
			if failures != nil {
				e.Every("notify-failures", notifyFailureEvery, failures.Run)
			}
		},
	}, nil
}
