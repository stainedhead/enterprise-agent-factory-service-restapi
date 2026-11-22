package collab

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Person is someone who should have a Paperclip account: the email and the Paperclip role.
type Person struct {
	Email string
	Role  InviteRole
}

// TeamPeople is the people of one managed team whose Paperclip is up.
type TeamPeople struct {
	TeamID string
	// Wanted are people with a user binding on the team and people who signed in through the edge in the
	// last 30 days (people who reach the team only through an IdP group are invited on their first sign-in).
	Wanted []Person
}

// PeopleSource reads the wanted people of every managed team with a ready pod and a recorded board key.
type PeopleSource interface {
	People(ctx context.Context) ([]TeamPeople, error)
}

// MemberSync is member sync (COL-33, managed mode): it invites the wanted people and deactivates the Paperclip
// accounts of people who are no longer wanted. Access is decided by the edge, so a stale account cannot be used.
// The admin invites are never touched: the first admin is the bootstrap's.
type MemberSync struct {
	Source PeopleSource
	Paper  Paperclip
	Mail   Mailer
	Log    *slog.Logger
}

func (m *MemberSync) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

// Run syncs every team once.
func (m *MemberSync) Run(ctx context.Context) error {
	teams, err := m.Source.People(ctx)
	if err != nil {
		return fmt.Errorf("collab: read people: %w", err)
	}
	var failed int
	for _, t := range teams {
		if err := m.team(ctx, t); err != nil {
			failed++
			m.log().Warn("member sync failed", "team", t.TeamID, "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("collab: member sync failed for %d teams", failed)
	}
	return nil
}

func (m *MemberSync) team(ctx context.Context, t TeamPeople) error {
	invites, err := m.Paper.Invites(ctx, t.TeamID)
	if err != nil {
		return err
	}
	have := map[string]InviteInfo{}
	for _, i := range invites {
		have[strings.ToLower(i.Email)] = i
	}
	want := map[string]bool{}
	var firstErr error
	for _, p := range t.Wanted {
		e := strings.ToLower(p.Email)
		want[e] = true
		if _, ok := have[e]; ok || e == "" {
			continue
		}
		if err := m.invite(ctx, t.TeamID, p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for e, i := range have {
		if want[e] || i.Role == RoleAdmin {
			continue
		}
		if err := m.Paper.DeactivateMember(ctx, t.TeamID, i.Email); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Withdrawing the invite too keeps the person out of the next pass's list and lets a later
		// re-binding invite them afresh.
		if err := m.Paper.RevokeInvite(ctx, t.TeamID, i.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *MemberSync) invite(ctx context.Context, teamID string, p Person) error {
	role := p.Role
	if role == "" {
		role = RoleMember
	}
	inv, err := m.Paper.CreateInvite(ctx, teamID, p.Email, role)
	if err != nil {
		return err
	}
	if err := m.Mail.Send(ctx, []string{p.Email}, "You are invited to your team's Paperclip",
		"You have been invited to your team's Paperclip. Accept the invite here:\n\n"+inv.URL.Reveal()+"\n"); err != nil {
		_ = m.Paper.RevokeInvite(ctx, teamID, inv.ID)
		return fmt.Errorf("send the invite: %w", err)
	}
	return nil
}
