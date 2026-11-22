package matrix

import (
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M2-A (catalogs II: models and prices, storage classes, agent templates, catalog systems).
// The catalogs are service-wide, so a destructive case makes its own uniquely named row in Prepare.

const digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// seedModel inserts an enabled model with a unique id and returns the id and its version.
func (w *World) seedModel(t *testing.T) (id, version string) {
	t.Helper()
	id = "mx-model-" + unique()
	w.exec(t, `INSERT INTO afs.models (id, provider, display_name, context_window, max_output_tokens) VALUES ($1, 'Example', 'Matrix model', 200000, 8192)`, id)
	return id, ver(t, w, `SELECT version FROM afs.models WHERE id = $1`, id)
}

// seedCatalogSystem inserts a service-level external system and returns its id and version.
func (w *World) seedCatalogSystem(t *testing.T) (id, version string) {
	t.Helper()
	id = pgtest.NewID("ext")
	w.exec(t, `INSERT INTO afs.external_systems (id, slug, name, category, endpoints) VALUES ($1, $2, 'Matrix system', 'other', ARRAY['mx.example.com'])`,
		id, "mx-cat-"+strings.ToLower(unique()))
	return id, ver(t, w, `SELECT version FROM afs.external_systems WHERE id = $1`, id)
}

func init() {
	for _, c := range []struct{ op, path string }{
		{"ListModels", "/v1/models"}, {"ListAdminModels", "/v1/admin/models"}, {"ListStorageClasses", "/v1/storage-classes"},
		{"ListStorageClassesAdmin", "/v1/admin/storage-classes"}, {"ListAgentTemplates", "/v1/agent-templates"},
		{"ListAdminAgentTemplates", "/v1/admin/agent-templates"}, {"ListCatalogSystems", "/v1/admin/external-systems"},
	} {
		Register(Case{Op: c.op, Path: c.path})
	}

	// Models and prices.
	Register(Case{Op: "CreateModel", Path: "/v1/admin/models", NoOutbox: cdOutbox,
		Body:    `{"id":"mx-new-{n}","provider":"Example","displayName":"Matrix","contextWindow":200000,"maxOutputTokens":8192}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": unique()} }})
	Register(Case{Op: "UpdateModel", Path: "/v1/admin/models/{mid}", Body: `{"displayName":"Renamed"}`, NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string {
			id, v := w.seedModel(t)
			return map[string]string{"mid": id, "ver": v}
		}})
	Register(Case{Op: "ListModelPrices", Path: "/v1/admin/models/{mid}/prices",
		Prepare: func(t *testing.T, w *World) map[string]string {
			id, _ := w.seedModel(t)
			return map[string]string{"mid": id}
		}})
	Register(Case{Op: "CreateModelPrice", Path: "/v1/admin/models/{mid}/prices", NoOutbox: cdOutbox,
		Body: `{"effectiveFrom":"{eff}","inputUsdPerMtok":"3.00","outputUsdPerMtok":"15.00","cacheReadUsdPerMtok":"0.30","cacheWriteUsdPerMtok":"3.75"}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			id, _ := w.seedModel(t)
			return map[string]string{"mid": id, "eff": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
		}})

	// Storage classes.
	Register(Case{Op: "PutStorageClass", Path: "/v1/admin/storage-classes/mx-put", Body: `{"description":"matrix","volumeType":"gp3"}`, NoOutbox: cdOutbox})
	Register(Case{Op: "DeleteStorageClass", Path: "/v1/admin/storage-classes/mx-del", NoOutbox: cdOutbox, Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.exec(t, `INSERT INTO afs.storage_classes (id, description, volume_type) VALUES ('mx-del', 'matrix', 'gp3') ON CONFLICT (id) DO NOTHING`)
			return map[string]string{"ver": ver(t, w, `SELECT version FROM afs.storage_classes WHERE id = 'mx-del'`)}
		}})

	// Agent templates.
	Register(Case{Op: "PutAgentTemplate", Path: "/v1/admin/agent-templates/mx-put", NoOutbox: cdOutbox,
		Body: `{"description":"matrix","harness":"hermes","defaultResources":{"cpu":"500m","memory":"1Gi"},"allowedDigests":["` + digestA + `"],"currentDigest":"` + digestA + `"}`})
	Register(Case{Op: "DeleteAgentTemplate", Path: "/v1/admin/agent-templates/mx-del", NoOutbox: cdOutbox, Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.exec(t, `INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest) VALUES ($1, 'mx-del', 'hermes', ARRAY[$2], $2) ON CONFLICT (name) DO NOTHING`,
				pgtest.NewID("tpl"), digestA)
			return map[string]string{"ver": ver(t, w, `SELECT version FROM afs.agent_templates WHERE name = 'mx-del'`)}
		}})

	// Catalog systems.
	Register(Case{Op: "CreateCatalogSystem", Path: "/v1/admin/external-systems", NoOutbox: cdOutbox,
		Body:    `{"slug":"mx-new-{n}","name":"Matrix","category":"other","endpoints":["mx.example.com"]}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": strings.ToLower(unique())} }})
	Register(Case{Op: "GetCatalogSystem", Path: "/v1/admin/external-systems/{sid}",
		Prepare: func(t *testing.T, w *World) map[string]string {
			id, _ := w.seedCatalogSystem(t)
			return map[string]string{"sid": id}
		}})
	catalogSystem := func(t *testing.T, w *World) map[string]string {
		id, v := w.seedCatalogSystem(t)
		return map[string]string{"sid": id, "ver": v}
	}
	Register(Case{Op: "UpdateCatalogSystem", Path: "/v1/admin/external-systems/{sid}", Body: `{"name":"Renamed"}`, NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: catalogSystem})
	Register(Case{Op: "DeleteCatalogSystem", Path: "/v1/admin/external-systems/{sid}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: catalogSystem})
}
