package httpapi

import (
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/me"
)

// principalDTO, meTeamDTO and meDTO are the Me schema of API contract §4.1.
type principalDTO struct {
	ID    string              `json:"id"`
	Type  authz.PrincipalType `json:"type"`
	Email string              `json:"email,omitempty"`
	Name  string              `json:"name,omitempty"`
}

type meTeamDTO struct {
	TeamID      string   `json:"teamId"`
	Slug        string   `json:"slug"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions"`
}

type meDTO struct {
	Principal          principalDTO `json:"principal"`
	ServiceAdmin       bool         `json:"serviceAdmin"`
	SecurityOperator   bool         `json:"securityOperator"`
	ServicePermissions []string     `json:"servicePermissions,omitempty"`
	Teams              []meTeamDTO  `json:"teams"`
}

func toMeDTO(r me.Result) meDTO {
	out := meDTO{
		Principal:          principalDTO{ID: r.Principal.ID, Type: r.Principal.Type, Email: r.Principal.Email, Name: r.Principal.Name},
		ServiceAdmin:       r.ServiceAdmin,
		SecurityOperator:   r.SecurityOperator,
		ServicePermissions: permStrings(r.ServicePermissions),
		Teams:              make([]meTeamDTO, 0, len(r.Teams)),
	}
	for _, t := range r.Teams {
		out.Teams = append(out.Teams, meTeamDTO{TeamID: t.TeamID, Slug: t.Slug, Role: t.Role, Permissions: permStrings(t.Permissions)})
	}
	return out
}

func permStrings(ps []authz.Permission) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}

// signInProviderDTO is the SignInProvider schema: never a client secret or its ARN.
type signInProviderDTO struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	IssuerURL       string `json:"issuerUrl"`
	ConsoleClientID string `json:"consoleClientId"`
	CLIClientID     string `json:"cliClientId"`
	IsDefault       bool   `json:"isDefault"`
}

type signInProviderList struct {
	Items []signInProviderDTO `json:"items"`
}

// toSignInProviders maps providers to the public list. Providers have one client id (CD-9), which is
// both the console and the CLI audience.
func toSignInProviders(ps []auth.Provider) signInProviderList {
	out := signInProviderList{Items: make([]signInProviderDTO, 0, len(ps))}
	for _, p := range ps {
		out.Items = append(out.Items, signInProviderDTO{ID: p.ID, Kind: p.Kind, Name: p.Name, IssuerURL: p.IssuerURL,
			ConsoleClientID: p.ClientID, CLIClientID: p.ClientID, IsDefault: p.IsDefault})
	}
	return out
}
