package eventbridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	eb "github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
)

// MaxEntryBytes is the EventBridge limit of one entry.
const MaxEntryBytes = 256 * 1024

// EntryError is the error of one rejected entry; Code is the EventBridge error code.
type EntryError struct {
	Code    string
	Message string
}

func (e *EntryError) Error() string { return fmt.Sprintf("eventbridge: %s: %s", e.Code, e.Message) }

// PutEvents implements housekeeping.EventSink: source "afs", detail-type the event type, detail the
// outbox payload. It returns one error per entry, in order.
func (c *Client) PutEvents(ctx context.Context, entries []housekeeping.Entry) ([]error, error) {
	if c.bus == "" {
		return nil, errors.New("eventbridge: event bus name is not configured")
	}
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) > housekeeping.PutEventsMax {
		return nil, fmt.Errorf("eventbridge: %d entries exceed the limit of %d", len(entries), housekeeping.PutEventsMax)
	}
	in := &eb.PutEventsInput{Entries: make([]types.PutEventsRequestEntry, len(entries))}
	for i, e := range entries {
		t := e.Time
		in.Entries[i] = types.PutEventsRequestEntry{
			EventBusName: aws.String(c.bus),
			Source:       aws.String(housekeeping.Source),
			DetailType:   aws.String(e.DetailType),
			Detail:       aws.String(string(e.Detail)),
			Resources:    e.Resources,
			Time:         &t,
		}
	}
	out, err := c.api.PutEvents(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("eventbridge: PutEvents: %w", err)
	}
	if len(out.Entries) != len(entries) {
		return nil, fmt.Errorf("eventbridge: PutEvents returned %d entries for %d", len(out.Entries), len(entries))
	}
	res := make([]error, len(entries))
	if out.FailedEntryCount == 0 {
		return res, nil
	}
	for i, r := range out.Entries {
		if r.ErrorCode != nil {
			res[i] = &EntryError{Code: aws.ToString(r.ErrorCode), Message: aws.ToString(r.ErrorMessage)}
		}
	}
	return res, nil
}

var _ housekeeping.EventSink = (*Client)(nil)
