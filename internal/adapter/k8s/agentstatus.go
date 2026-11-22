package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/activity"
)

// Pinned defaults of the agent status endpoint (OQ-7). The Agentic Teammates runtime serves
// GET /status on the pod's status port; the body is {"state": "busy" | "idle", "activity": "<note>"}.
// When the runtime pins another shape, only this file changes.
const (
	// DefaultStatusPort is the pod port of the status endpoint.
	DefaultStatusPort = 8089
	// StatusPath is the endpoint's path.
	StatusPath = "/status"
	// statusTimeout bounds one call; the collector falls back to CPU and model calls when it expires.
	statusTimeout = 2 * time.Second
	// statusBodyLimit bounds what is read from a pod, which is untrusted.
	statusBodyLimit = 4 << 10
)

// AgentStatus implements activity.StatusSource over HTTP.
type AgentStatus struct {
	client *http.Client
	port   int
}

// NewAgentStatus returns an AgentStatus for the status port (0 for the default). A nil client gets one
// with the call timeout and no redirects.
func NewAgentStatus(client *http.Client, port int) *AgentStatus {
	if client == nil {
		client = &http.Client{Timeout: statusTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if port <= 0 {
		port = DefaultStatusPort
	}
	return &AgentStatus{client: client, port: port}
}

var _ activity.StatusSource = (*AgentStatus)(nil)

// Report implements activity.StatusSource. A pod with no address, no listener or no such path is
// activity.ErrNoStatus; so is an answer that is not the pinned shape.
func (a *AgentStatus) Report(ctx context.Context, p activity.Pod) (activity.StatusReport, error) {
	if p.IP == "" {
		return activity.StatusReport{}, activity.ErrNoStatus
	}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	url := "http://" + net.JoinHostPort(p.IP, strconv.Itoa(a.port)) + StatusPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return activity.StatusReport{}, err
	}
	resp, err := a.client.Do(req) //nolint:gosec // the address is a pod IP the cluster API returned, on the pinned status port
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
			return activity.StatusReport{}, activity.ErrNoStatus
		}
		return activity.StatusReport{}, fmt.Errorf("k8s: agent status %s: %w", p.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return activity.StatusReport{}, activity.ErrNoStatus
	}
	var body struct {
		State    string `json:"state"`
		Activity string `json:"activity"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, statusBodyLimit)).Decode(&body); err != nil {
		return activity.StatusReport{}, activity.ErrNoStatus
	}
	switch body.State {
	case "busy", "idle":
	default:
		return activity.StatusReport{}, activity.ErrNoStatus
	}
	return activity.StatusReport{Busy: body.State == "busy", Note: body.Activity}, nil
}
