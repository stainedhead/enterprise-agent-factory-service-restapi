package main

import (
	"context"
	"log/slog"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/secretsmanager"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/ses"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sqs"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/github"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

// Environment of the work order reconciler (M6-E, RC-12).
const (
	// envWorkOrderQueueURL is the URL of the SQS queue the EventBridge rule for afs.work_order.requested targets.
	// Without it intake is off (work orders still come from the API).
	envWorkOrderQueueURL = "AFS_WORK_ORDER_QUEUE_URL"
	// envGitHubAppsFile is the apps file (internal/adapter/github.LoadApps): one GitHub App per organization.
	// Without it pull request state is not read; agents' reports alone move repositories.
	envGitHubAppsFile = "AFS_GITHUB_APPS_FILE"
)

// contactMailer emails an outside contact. The channel that reached it (work_orders.notification_channel_id, an
// enabled service-level email channel, checked by the store) is the permission to send; the sender is the
// notification sender address, AFS_NOTIFY_EMAIL_FROM.
type contactMailer struct{ mail *ses.Mailer }

func (m contactMailer) Send(ctx context.Context, _, to, subject, body string) error {
	return m.mail.Send(ctx, []string{to}, subject, body)
}

// wireWorkOrders registers the work order jobs on the engine's leader-only scheduler: intake, team creation
// with template secrets, pull request polling and status settling, and owner notifications.
func wireWorkOrders(s workorder.Scheduler, awsCfg awssdk.Config, runner port.TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen,
	env func(string) string, now func() time.Time, log *slog.Logger) error {
	store := postgres.NewWorkOrderStore(runner, audit, outbox, ids, now)
	jobs := workorder.Jobs{
		Assigner: &workorder.Assigner{Store: store, Now: now, Log: log,
			Secrets: &workorder.Secrets{Store: store, Creator: secretsmanager.NewEmptyCreator(awsCfg), Log: log}},
		Tracker:  &workorder.Tracker{Store: store, Now: now, Log: log},
		Notifier: &workorder.Notifier{Source: store, Store: store, Log: log},
	}
	if url := env(envWorkOrderQueueURL); url != "" {
		q, err := sqs.NewIntake(awsCfg, url)
		if err != nil {
			return err
		}
		jobs.Intake = &workorder.Intake{Queue: q, Store: store, Now: now, Log: log}
	} else {
		log.Warn("work order intake from EventBridge is off: " + envWorkOrderQueueURL + " is not set")
	}
	if file := env(envGitHubAppsFile); file != "" {
		apps, err := github.LoadApps(file)
		if err != nil {
			return err
		}
		jobs.Tracker.GitHub = github.New(github.Options{Apps: apps, Now: now})
		log.Info("GitHub pull requests are read", "installations", len(apps))
	} else {
		log.Warn("GitHub pull requests are not read: " + envGitHubAppsFile + " is not set")
	}
	if from := env(envNotifyEmailFrom); from != "" {
		jobs.Notifier.Mailer = contactMailer{mail: ses.New(awsCfg, from)}
	} else {
		log.Warn("outside contacts of work orders are not emailed: " + envNotifyEmailFrom + " is not set")
	}
	workorder.Register(s, jobs)
	return nil
}
