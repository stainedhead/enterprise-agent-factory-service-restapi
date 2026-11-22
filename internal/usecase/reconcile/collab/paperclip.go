package collab

import (
	"context"
	"log/slog"
	"time"
)

// Key is a Paperclip credential held in memory: an agent key, the board key or an invite URL (which
// is a credential too, COL-32). It prints as [redacted] everywhere; only the adapter that hands it to
// Secrets Manager, Paperclip or the mailer calls Reveal.
type Key string

// String implements fmt.Stringer.
func (Key) String() string { return "[redacted]" }

// GoString implements fmt.GoStringer.
func (Key) GoString() string { return "collab.Key([redacted])" }

// LogValue implements slog.LogValuer.
func (Key) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// MarshalJSON keeps a Key out of any encoded value.
func (Key) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Reveal returns the value.
func (k Key) Reveal() string { return string(k) }

// RosterAgent is an agent in Paperclip's roster.
type RosterAgent struct {
	ID, Name         string
	Orchestrator     bool
	LocalAdapter     bool
	Paused           bool
	LinkedAFSAgentID string
}

// Item is an issue or comment of Paperclip with its text. The text exists only while it is scanned.
type Item struct {
	ID, AuthorAgentID string
	UpdatedAt         time.Time
	Text              string
}

// NewAgent describes the Paperclip agent created for a connected AFS worker (COL-12).
type NewAgent struct {
	Name, AFSAgentID, Persona string
}

// CreatedAgent is the Paperclip agent and the key Paperclip returned once.
type CreatedAgent struct {
	ID  string
	Key Key
}

// InviteRole is the role of an invited person in Paperclip.
type InviteRole string

// InviteRole values.
const (
	RoleAdmin  InviteRole = "admin"
	RoleMember InviteRole = "member"
	RoleViewer InviteRole = "viewer"
)

// InviteInfo is an invite as the roster of invites lists it. It carries no URL.
type InviteInfo struct {
	ID, Email string
	Role      InviteRole
	Accepted  bool
}

// Invite is a created invite; URL is a credential.
type Invite struct {
	ID  string
	URL Key
}

// Status is what Paperclip answers about itself.
type Status struct {
	// SignUpOpen is true when anyone can create an account (COL-27 requires it closed).
	SignUpOpen bool
}

// Reader is the read side of Paperclip the roster guard and the content scan use.
type Reader interface {
	Roster(ctx context.Context, teamID string) ([]RosterAgent, error)
	Changed(ctx context.Context, teamID string, since time.Time) ([]Item, error)
}

// Paperclip is the reconciler's view of a team's Paperclip API (managed mode only). The adapter
// (adapter/paperclip) authenticates with the team's board key and never logs a credential.
type Paperclip interface {
	// Status calls the health endpoint; an error means Paperclip did not answer.
	Status(ctx context.Context, teamID string) (Status, error)
	// Roster lists the live (not revoked) agents.
	Roster(ctx context.Context, teamID string) ([]RosterAgent, error)
	// Changed returns the issues and comments created or changed after since, oldest first.
	Changed(ctx context.Context, teamID string, since time.Time) ([]Item, error)
	// CreateAgent creates a remote-adapter agent and returns its key (shown by Paperclip once).
	CreateAgent(ctx context.Context, teamID string, a NewAgent) (CreatedAgent, error)
	// SetAgentPaused pauses or resumes an agent.
	SetAgentPaused(ctx context.Context, teamID, agentID string, paused bool) error
	// RevokeAgent revokes the agent's key and removes it from the roster. A missing agent is success.
	RevokeAgent(ctx context.Context, teamID, agentID string) error
	// Invites lists the invites and their state.
	Invites(ctx context.Context, teamID string) ([]InviteInfo, error)
	// CreateInvite invites email in role.
	CreateInvite(ctx context.Context, teamID, email string, role InviteRole) (Invite, error)
	// RevokeInvite withdraws an invite. A missing invite is success.
	RevokeInvite(ctx context.Context, teamID, inviteID string) error
	// DeactivateMember deactivates the account of email. A missing account is success.
	DeactivateMember(ctx context.Context, teamID, email string) error
}

// Mailer sends the one-off emails that carry an invite URL (the reconciler's notification delivery,
// RC-15). The body is never logged.
type Mailer interface {
	Send(ctx context.Context, to []string, subject, body string) error
}
