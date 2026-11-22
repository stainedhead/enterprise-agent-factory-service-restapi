package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

func TestWireNotifyIsInertWithoutTheTargetRole(t *testing.T) {
	var logged strings.Builder
	w, err := wireNotify(notifyDeps{Env: func(string) string { return "" }, Now: time.Now, Log: slog.New(slog.NewTextHandler(&logged, nil))})
	if err != nil || w.Controller != nil || len(w.Routes) != 0 || !strings.Contains(logged.String(), envNotifyTargetRole) {
		t.Fatalf("%+v %v %s", w, err, logged.String())
	}
	var l engine.Lister
	if w.WrapLister(l) != l {
		t.Fatal("an inert wiring must not wrap the lister")
	}
	w.Register(nil) // must not touch the engine
}

func TestWireNotifyRejectsABadQueueARN(t *testing.T) {
	env := map[string]string{envNotifyTargetRole: "arn:aws:iam::111122223333:role/afs-notify", envNotifyEmailQueue: "not-an-arn"}
	_, err := wireNotify(notifyDeps{Env: func(k string) string { return env[k] }, Now: time.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil {
		t.Fatal("want error")
	}
}

func TestWireNotifyBuildsControllerAndRoute(t *testing.T) {
	env := map[string]string{envNotifyTargetRole: "arn:aws:iam::111122223333:role/afs-notify",
		envNotifyEmailQueue: "arn:aws:sqs:us-east-1:111122223333:afs-email.fifo", envNotifyDLQ: "arn:aws:sqs:us-east-1:111122223333:afs-dlq"}
	w, err := wireNotify(notifyDeps{Env: func(k string) string { return env[k] }, Now: time.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil || w.Controller == nil || w.Controller.Kind() != notify.Kind {
		t.Fatalf("%+v %v", w, err)
	}
	// The channel route merges with the others into one registry (a table is routed once).
	if _, err := routes(w.Routes...); err != nil {
		t.Fatal(err)
	}
}
