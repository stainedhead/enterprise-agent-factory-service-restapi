package httpapi

import (
	"context"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/me"
)

// MeUseCase is GetMe (RA-3).
type MeUseCase interface {
	Get(ctx context.Context, p authz.Principal, requestID string) (me.Result, error)
}

// ProviderLister lists the enabled identity providers for the sign-in screen (RA-US-2.5).
type ProviderLister interface {
	SignInProviders(ctx context.Context) ([]auth.Provider, error)
}

// MeRegistrar binds GetMe and ListSignInProviders.
func MeRegistrar(uc MeUseCase, providers ProviderLister) Registrar {
	return func(g *Registry) error {
		if err := g.Handle("GetMe", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				WriteError(w, r, domain.Unauthenticated())
				return
			}
			res, err := uc.Get(r.Context(), p, RequestIDFrom(r.Context()))
			if err != nil {
				WriteError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, toMeDTO(res))
		})); err != nil {
			return err
		}
		return g.Handle("ListSignInProviders", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ps, err := providers.SignInProviders(r.Context())
			if err != nil {
				WriteError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, toSignInProviders(ps))
		}))
	}
}
