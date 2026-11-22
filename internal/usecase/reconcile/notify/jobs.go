package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// TestRequest is a notification.test_requested event waiting for its result.
type TestRequest struct {
	ChannelID string
	TestID    string
	At        time.Time
}

// TestRequests lists the test requests written since a time.
type TestRequests interface {
	PendingTests(ctx context.Context, since time.Time) ([]TestRequest, error)
}

// TestWindow is how far back a test request is still honored.
const TestWindow = 2 * time.Minute

// TestRunner delivers the tests TestNotificationChannel asked for and records each result on the
// channel (OQ-S1). It runs as a periodic job on the leader.
type TestRunner struct {
	Requests  TestRequests
	Source    Source
	Secrets   Secrets
	Deliverer Deliverer
	Recorder  Recorder
	Events    Events
	Now       func() time.Time
	Log       *slog.Logger

	mu   sync.Mutex
	done map[string]time.Time
}

func (t *TestRunner) seen(id string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done == nil {
		t.done = map[string]time.Time{}
	}
	for k, at := range t.done {
		if now.Sub(at) > 2*TestWindow {
			delete(t.done, k)
		}
	}
	if _, ok := t.done[id]; ok {
		return true
	}
	t.done[id] = now
	return false
}

// Run handles every pending request once.
func (t *TestRunner) Run(ctx context.Context) error {
	now := t.Now()
	reqs, err := t.Requests.PendingTests(ctx, now.Add(-TestWindow))
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range reqs {
		if t.seen(r.TestID, now) {
			continue
		}
		if err := t.handle(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (t *TestRunner) handle(ctx context.Context, r TestRequest) error {
	ch, err := t.Source.LoadChannel(ctx, r.ChannelID)
	if err != nil {
		return nil //nolint:nilerr // the channel is gone: nothing to test or record
	}
	if domain.DeliveryDetailOf(ch.Prior).LastTestID == r.TestID {
		return nil // already recorded (a restart of the reconciler)
	}
	var cred Credential
	var sendErr error
	if needsCredential(ch.Kind) && ch.SecretARN != "" {
		if cred, sendErr = t.Secrets.Read(ctx, ch.SecretARN); sendErr != nil {
			sendErr = errors.New("the channel's secret could not be read")
		}
	}
	if sendErr == nil {
		sendErr = t.Deliverer.Deliver(ctx, ch, cred, TestMessage(ch, r.TestID, t.Now()))
	}
	d := Delivery{At: t.Now(), OK: sendErr == nil, TestID: r.TestID}
	if sendErr != nil {
		d.Err = sendErr.Error()
	}
	if err := t.Recorder.Record(ctx, ch, d); err != nil {
		return fmt.Errorf("record test %s: %w", r.TestID, err)
	}
	if sendErr != nil && t.Events != nil {
		if err := t.Events.DeliveryFailed(ctx, ch, RedactURLs(d.Err), string(domain.EventNotificationTestRequested)); err != nil {
			t.Log.Warn("emit delivery failure", slog.String("channel", ch.ID), slog.Any("err", err))
		}
	}
	return nil
}

// QueueMessage is a message of an SQS queue.
type QueueMessage struct {
	ID      string
	Receipt string
	Body    string
	Attrs   map[string]string
}

// Queue is the subset of SQS the drainers use.
type Queue interface {
	Receive(ctx context.Context, max int) ([]QueueMessage, error)
	Delete(ctx context.Context, m QueueMessage) error
}

// FailureDrainer turns the messages of the EventBridge dead-letter queue (events whose delivery
// failed after EventBridge's retries) into channel status and notification.delivery_failed events
// (RC-US-13.3).
type FailureDrainer struct {
	Queue    Queue
	Source   Source
	Recorder Recorder
	Events   Events
	Now      func() time.Time
	Log      *slog.Logger
}

// Run drains the queue once (up to a bounded number of batches).
func (f *FailureDrainer) Run(ctx context.Context) error {
	for range 10 {
		msgs, err := f.Queue.Receive(ctx, 10)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
		for _, m := range msgs {
			if err := f.handle(ctx, m); err != nil {
				f.Log.Warn("dead-letter message not handled", slog.String("message", m.ID), slog.Any("err", err))
				continue
			}
			if err := f.Queue.Delete(ctx, m); err != nil {
				f.Log.Warn("delete dead-letter message", slog.String("message", m.ID), slog.Any("err", err))
			}
		}
	}
	return nil
}

func (f *FailureDrainer) handle(ctx context.Context, m QueueMessage) error {
	rule := m.Attrs["RULE_ARN"]
	name := rule[strings.LastIndex(rule, "/")+1:]
	id, ok := ChannelIDOf(name)
	if !ok {
		return nil // not one of ours: drop it
	}
	ch, err := f.Source.LoadChannel(ctx, id)
	if err != nil {
		if errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
			return nil // the channel is gone
		}
		return err
	}
	text := strings.TrimSpace(m.Attrs["ERROR_CODE"] + ": " + m.Attrs["ERROR_MESSAGE"])
	if text == ":" {
		text = "delivery failed"
	}
	text = RedactURLs(text)
	var ev struct {
		DetailType string `json:"detail-type"`
	}
	_ = json.Unmarshal([]byte(m.Body), &ev)
	if err := f.Recorder.Record(ctx, ch, Delivery{At: f.Now(), Err: text}); err != nil {
		return err
	}
	return f.Events.DeliveryFailed(ctx, ch, text, ev.DetailType)
}

// emailEnvelope is the body of a message the email rule's input transformer puts on the queue.
type emailEnvelope struct {
	ChannelID  string          `json:"channelId"`
	EventID    string          `json:"eventId"`
	DetailType string          `json:"detailType"`
	Detail     json.RawMessage `json:"detail"`
}

// EmailDrainer sends the events queued for email channels with SES, once per (channel, event id): a
// duplicate delivery of the same event, from EventBridge or from SQS redelivery, sends nothing more
// (RC-US-13.2). The queue is FIFO with content-based deduplication, which covers duplicates across a
// restart inside SQS's window; the in-memory set covers redelivery inside this process.
type EmailDrainer struct {
	Queue    Queue
	Source   Source
	Mailer   Mailer
	Recorder Recorder
	Events   Events
	Now      func() time.Time
	Log      *slog.Logger

	mu   sync.Mutex
	sent map[string]time.Time
}

// emailSeenTTL and emailSeenMax bound the in-memory duplicate set.
const (
	emailSeenTTL = 24 * time.Hour
	emailSeenMax = 20000
	// failuresBeforeEvent is the SQS receive count at which a failing email raises delivery_failed.
	failuresBeforeEvent = 3
)

func (e *EmailDrainer) already(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.sent[key]
	return ok
}

func (e *EmailDrainer) mark(key string, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sent == nil {
		e.sent = map[string]time.Time{}
	}
	if len(e.sent) >= emailSeenMax {
		for k, at := range e.sent {
			if now.Sub(at) > emailSeenTTL {
				delete(e.sent, k)
			}
		}
		if len(e.sent) >= emailSeenMax { // still full: drop the oldest tenth
			type kv struct {
				k string
				t time.Time
			}
			all := make([]kv, 0, len(e.sent))
			for k, t := range e.sent {
				all = append(all, kv{k, t})
			}
			sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
			for _, x := range all[:len(all)/10+1] {
				delete(e.sent, x.k)
			}
		}
	}
	e.sent[key] = now
}

// Run drains the queue once (up to a bounded number of batches).
func (e *EmailDrainer) Run(ctx context.Context) error {
	for range 10 {
		msgs, err := e.Queue.Receive(ctx, 10)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
		for _, m := range msgs {
			if done, err := e.handle(ctx, m); err != nil {
				e.Log.Warn("email not sent", slog.String("message", m.ID), slog.Any("err", RedactURLs(err.Error())))
			} else if done {
				if err := e.Queue.Delete(ctx, m); err != nil {
					e.Log.Warn("delete email message", slog.String("message", m.ID), slog.Any("err", err))
				}
			}
		}
	}
	return nil
}

// handle returns done when the message may be deleted.
func (e *EmailDrainer) handle(ctx context.Context, m QueueMessage) (bool, error) {
	var env emailEnvelope
	if err := json.Unmarshal([]byte(m.Body), &env); err != nil || env.ChannelID == "" || env.EventID == "" {
		return true, nil // not ours or malformed: drop it
	}
	now := e.Now()
	key := env.ChannelID + "/" + env.EventID
	if e.already(key) {
		return true, nil
	}
	ch, err := e.Source.LoadChannel(ctx, env.ChannelID)
	if err != nil {
		if errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
			return true, nil
		}
		return false, err
	}
	if ch.Kind != domain.ChannelEmail || !ch.Enabled {
		return true, nil
	}
	subject := "[AFS] " + env.DetailType
	if p := domain.ConfigString(ch.Config, "subjectPrefix"); p != "" {
		subject = p + " " + env.DetailType
	}
	pretty, _ := json.MarshalIndent(json.RawMessage(env.Detail), "", "  ")
	body := fmt.Sprintf("%s\nEvent id: %s\n\n%s\n", env.DetailType, env.EventID, pretty)
	if err := e.Mailer.Send(ctx, domain.Recipients(ch.Config), subject, body); err != nil {
		text := RedactURLs(err.Error())
		_ = e.Recorder.Record(ctx, ch, Delivery{At: now, Err: text})
		if n, _ := strconv.Atoi(m.Attrs["ApproximateReceiveCount"]); n >= failuresBeforeEvent {
			_ = e.Events.DeliveryFailed(ctx, ch, text, env.DetailType)
		}
		return false, err // stays on the queue; SQS redelivers
	}
	e.mark(key, now)
	if err := e.Recorder.Record(ctx, ch, Delivery{At: now, OK: true}); err != nil {
		e.Log.Warn("record delivery", slog.String("channel", ch.ID), slog.Any("err", err))
	}
	return true, nil
}

// Sweeper removes the rules of channels that no longer exist (a missed delete notification, a channel
// removed with its team): the backstop of "removed within 60 s of delete" (RC-US-13.4).
type Sweeper struct {
	Bus     Bus
	Source  Source
	Cleaner StatusCleaner
	Log     *slog.Logger
}

// Run lists the rules and removes the orphaned ones.
func (s *Sweeper) Run(ctx context.Context) error {
	ids, err := s.Bus.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		ch, err := s.Source.LoadChannel(ctx, id)
		switch {
		case err == nil && ch.TeamState != "deleting" && ch.TeamState != "deleted":
			continue
		case err != nil && !errors.Is(err, &domain.Error{Kind: domain.KindNotFound}):
			errs = append(errs, err)
			continue
		}
		if err := s.Bus.Remove(ctx, id); err != nil {
			errs = append(errs, err)
			continue
		}
		if s.Cleaner != nil && err != nil {
			_ = s.Cleaner.DeleteStatus(ctx, id)
		}
		s.Log.Info("removed an orphaned notification rule", slog.String("channel", id))
	}
	return errors.Join(errs...)
}
