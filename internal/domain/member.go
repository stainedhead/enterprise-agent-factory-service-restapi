package domain

import (
	"net/mail"
	"strings"
	"time"
)

// Member is a role binding of a subject on a team (API contract §5).
type Member struct {
	ID                 string
	TeamID             string
	Role               TeamRole
	SubjectType        SubjectType
	UserID             string
	Email              string
	GroupName          string
	IdentityProviderID string
	Version            Version
	CreatedAt          time.Time
	CreatedBy          string
	UpdatedAt          time.Time
}

// NewMemberParams is an AddMember body. A user subject names userId or email, a group subject a
// groupName.
type NewMemberParams struct {
	Role               TeamRole
	SubjectType        SubjectType
	UserID             string
	Email              string
	GroupName          string
	IdentityProviderID string
}

// Validate checks the shape of an AddMember body: team roles only, and the subject fields that go
// with the subject type.
func (p NewMemberParams) Validate() error {
	var fe []FieldError
	if !p.Role.Valid() {
		fe = append(fe, FieldError{Field: "role", Reason: "must be team_admin or team_viewer"})
	}
	switch p.SubjectType {
	case SubjectTypeUser:
		switch {
		case p.GroupName != "":
			fe = append(fe, FieldError{Field: "groupName", Reason: "is only for a group subject"})
		case p.IdentityProviderID != "":
			fe = append(fe, FieldError{Field: "identityProviderId", Reason: "is only for a group subject"})
		case (p.UserID == "") == (p.Email == ""):
			fe = append(fe, FieldError{Field: "userId", Reason: "give exactly one of userId and email"})
		case p.UserID != "":
			if _, err := ParseID(PrefixUser, p.UserID); err != nil {
				fe = append(fe, FieldError{Field: "userId", Reason: "must be a usr_ identifier"})
			}
		default:
			if a, err := mail.ParseAddress(p.Email); err != nil || a.Address != p.Email {
				fe = append(fe, FieldError{Field: "email", Reason: "must be an email address"})
			}
		}
	case SubjectTypeGroup:
		if p.UserID != "" || p.Email != "" {
			fe = append(fe, FieldError{Field: "userId", Reason: "is only for a user subject"})
		}
		if n := len(strings.TrimSpace(p.GroupName)); n < 1 || n > 200 {
			fe = append(fe, FieldError{Field: "groupName", Reason: "is required for a group subject (1 to 200 characters)"})
		}
	default:
		fe = append(fe, FieldError{Field: "subjectType", Reason: "must be user or group"})
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// LastAdminRemoval is the Conflict for removing or demoting a team's only team-admin binding.
func LastAdminRemoval() *Error {
	return Conflict("last_team_admin", "a team keeps at least one team-admin binding", nil)
}
