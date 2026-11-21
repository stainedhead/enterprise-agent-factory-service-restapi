package matrix

// Cases of M1-A: caller identity and sign-in.
func init() {
	Register(Case{Op: "GetHealth", Path: "/healthz"})
	Register(Case{Op: "GetReadiness", Path: "/readyz"})
	Register(Case{Op: "GetMe", Path: "/v1/me"})
	Register(Case{Op: "ListSignInProviders", Path: "/v1/identity-providers"})
}
