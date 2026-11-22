package domain

// NewCatalogSystemParams are the fields of CreateCatalogSystem. Enabled defaults to true. Owner and
// purpose are not asked of a service-level entry.
type NewCatalogSystemParams struct {
	Slug        string
	Name        string
	Category    ExtCategory
	Description string
	DocsURL     string
	Endpoints   []string
	Enabled     *bool
}

// NewCatalogSystem validates p and returns a service-level system. Built-in entries ship with the
// schema, so one created through the API is never built in.
func NewCatalogSystem(p NewCatalogSystemParams) (ExternalSystem, error) {
	fe := validateExtCommon(nil, &p.Slug, &p.Name, &p.Category, &p.Description, &p.DocsURL, p.Endpoints, true)
	if len(fe) > 0 {
		return ExternalSystem{}, Validation(fe...)
	}
	s := ExternalSystem{Slug: p.Slug, Name: p.Name, Category: p.Category, Description: p.Description, DocsURL: p.DocsURL,
		Endpoints: p.Endpoints, Enabled: true}
	if p.Enabled != nil {
		s.Enabled = *p.Enabled
	}
	return s, nil
}
