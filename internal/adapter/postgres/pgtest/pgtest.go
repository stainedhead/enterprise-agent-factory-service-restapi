// Package pgtest gives integration tests a fresh, migrated PostgreSQL database per test package,
// with one LOGIN user per service role (afs_owner, afs_app, afs_reconciler, afs_gateway,
// afs_billing, afs_edge). Tests skip when AFS_TEST_DATABASE_URL is unset.
//
// Use it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }
//
// and call pgtest.Get(t) in each test.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stainedhead/enterprise-agent-factory-service-db/bootstrap"
	"github.com/stainedhead/enterprise-agent-factory-service-db/migrate"
	"github.com/stainedhead/enterprise-agent-factory-service-db/migrations"
)

// EnvURL names the variable holding the admin connection string.
const EnvURL = "AFS_TEST_DATABASE_URL"

// Roles are the service roles that get a LOGIN user, in order.
var Roles = []string{"afs_owner", "afs_app", "afs_reconciler", "afs_gateway", "afs_billing", "afs_edge"}

// DB is one package's test database.
type DB struct {
	// Name is the database name (prefix afsr_, so it never collides with -db tests).
	Name string
	// AdminURL connects as the admin (superuser) to this database.
	AdminURL string
	// URLs maps each role of Roles to a connection string of its LOGIN user.
	URLs map[string]string

	users    []string
	adminSrv string
}

var (
	once   sync.Once
	shared *DB
	setErr error

	strictOnce   sync.Once
	strictShared *DB
	strictErr    error
)

// legacyFixtureDefaults restores the world the 1.0.1 fixtures assume: a team may be inserted without an
// orchestration agent, a new team's endpoint limit and Web UI default to public (the 1.0.1 behaviour of
// the boolean columns) and an agent can be driven to deleting on a live team. The real schema is what
// GetStrict returns, and every 000022 test uses it (same shim as the -db suites, OM-D14).
var legacyFixtureDefaults = []string{
	`ALTER TABLE afs.teams ALTER COLUMN orchestration_required SET DEFAULT false`,
	`ALTER TABLE afs.teams ALTER COLUMN max_collaboration_exposure SET DEFAULT 'public'`,
	`ALTER TABLE afs.team_collaboration ALTER COLUMN web_ui_exposure SET DEFAULT 'public'`,
	`ALTER TABLE afs.agents DISABLE TRIGGER agents_orchestration_required`,
}

// Run runs the package's tests and drops the database afterwards.
func Run(m *testing.M) int {
	code := m.Run()
	for _, d := range []*DB{shared, strictShared} {
		if d == nil {
			continue
		}
		if err := d.drop(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "pgtest: cleanup:", err)
		}
	}
	return code
}

// Get returns the package's database, creating and migrating it on first use. It skips t when
// AFS_TEST_DATABASE_URL is unset.
func Get(t testing.TB) *DB {
	t.Helper()
	admin := os.Getenv(EnvURL)
	if admin == "" {
		t.Skip(EnvURL + " not set")
	}
	once.Do(func() { shared, setErr = create(admin, true) })
	if setErr != nil {
		t.Fatalf("pgtest: %v", setErr)
	}
	return shared
}

// GetStrict returns a second, separate database exactly as the migrations leave it: a new team needs
// an orchestration agent at commit, and the delete guard is on. Use it for the 000022 behaviour.
func GetStrict(t testing.TB) *DB {
	t.Helper()
	admin := os.Getenv(EnvURL)
	if admin == "" {
		t.Skip(EnvURL + " not set")
	}
	strictOnce.Do(func() { strictShared, strictErr = create(admin, false) })
	if strictErr != nil {
		t.Fatalf("pgtest: %v", strictErr)
	}
	return strictShared
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func withDB(raw, db, user, password string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String(), nil
}

func create(admin string, legacy bool) (*DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	suffix := strings.ToLower(randHex(4))
	d := &DB{Name: "afsr_" + suffix, URLs: map[string]string{}, adminSrv: admin}
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, bootstrap.RolesSQL); err != nil {
		return nil, fmt.Errorf("bootstrap roles: %w", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{d.Name}.Sanitize()+" OWNER afs_owner"); err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	if d.AdminURL, err = withDB(admin, d.Name, "", ""); err != nil {
		return nil, err
	}
	for _, role := range Roles {
		user := d.Name + "_" + strings.TrimPrefix(role, "afs_")
		pw := randHex(16)
		q := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' IN ROLE %s",
			pgx.Identifier{user}.Sanitize(), pw, pgx.Identifier{role}.Sanitize())
		if _, err := conn.Exec(ctx, q); err != nil {
			return nil, fmt.Errorf("create login %s: %w", user, err)
		}
		d.users = append(d.users, user)
		if d.URLs[role], err = withDB(admin, d.Name, user, pw); err != nil {
			return nil, err
		}
	}
	// Migrate as the owner login, as afs-api does at startup.
	oc, err := pgx.Connect(ctx, d.URLs["afs_owner"])
	if err != nil {
		return nil, fmt.Errorf("connect as owner: %w", err)
	}
	defer func() { _ = oc.Close(context.Background()) }()
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		return nil, err
	}
	if _, err := migrate.Up(ctx, oc, migs); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if legacy {
		for _, q := range legacyFixtureDefaults {
			if _, err := oc.Exec(ctx, q); err != nil {
				return nil, fmt.Errorf("legacy fixture defaults: %w", err)
			}
		}
	}
	return d, nil
}

func (d *DB) drop(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, d.adminSrv)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{d.Name}.Sanitize()+" WITH (FORCE)"); err != nil {
		return err
	}
	for _, u := range d.users {
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{u}.Sanitize()); err != nil {
			return err
		}
	}
	return nil
}

// Admin opens a superuser connection to the test database; it is closed when t ends. Use it to seed
// rows that bypass row-level security.
func (d *DB) Admin(t testing.TB) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), d.AdminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// NewID returns a fresh prefixed ULID-shaped id (random body) for seeding.
func NewID(prefix string) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := make([]byte, 26)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%32]
	}
	b[0] = '0'
	return prefix + "_" + string(b)
}

// Team is a seeded team.
type Team struct {
	ID, Slug string
}

// SeedTeam inserts an active team with a unique slug and returns it.
func (d *DB) SeedTeam(t testing.TB) Team {
	t.Helper()
	tm := Team{ID: NewID("team"), Slug: "t" + strings.ToLower(randHex(5))}
	_, err := d.Admin(t).Exec(t.Context(), `INSERT INTO afs.teams (id, slug, display_name, cost_center, owner_email, max_budget_usd)
VALUES ($1, $2, $2, 'CC100', 'owner@example.com', 1000)`, tm.ID, tm.Slug)
	if err != nil {
		t.Fatalf("seed team: %v", err)
	}
	return tm
}

// OrchestrationModel is the id of the priced model SeedOrchestrationRuntime adds.
const OrchestrationModel = "pgtest.orchestration-model"

var orchSeed sync.Map // *DB -> *sync.Once

// SeedOrchestrationRuntime gives the database what CreateTeam needs to create the orchestration agent: a
// priced model to default to, and a Paperclip template and image repository in orchestration.tools (a fresh
// database has none, so create_orchestration_agent answers orchestration_runtime_unset). It runs once per
// database; the database is dropped with the package, so nothing is cleaned up.
func (d *DB) SeedOrchestrationRuntime(t testing.TB) {
	t.Helper()
	once, _ := orchSeed.LoadOrStore(d, &sync.Once{})
	once.(*sync.Once).Do(func() {
		c := d.Admin(t)
		const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
		tpl := NewID("tpl")
		for _, q := range []string{
			`INSERT INTO afs.models (id, provider, display_name) VALUES ('` + OrchestrationModel + `', 'anthropic', 'Orchestration test model')`,
			`INSERT INTO afs.model_prices (model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok) VALUES ('` + OrchestrationModel + `', now() - interval '1 day', 3, 15)`,
			`INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest, capabilities, default_resources)
VALUES ('` + tpl + `', 'pgtest-orchestration-paperclip', 'paperclip', ARRAY['` + digest + `'], '` + digest + `', '{orchestration,tool:paperclip}', '{"cpu": "2", "memory": "8Gi"}')`,
			`UPDATE afs.service_settings SET value = jsonb_set(jsonb_set(value, '{paperclip,templateId}', to_jsonb('` + tpl + `'::text)),
 '{paperclip,imageRepository}', '"registry.example.com/paperclip"') WHERE key = 'orchestration.tools'`,
		} {
			if _, err := c.Exec(t.Context(), q); err != nil {
				t.Fatalf("seed orchestration runtime: %v\n%s", err, q)
			}
		}
	})
}
