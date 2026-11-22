// Package pgtest gives integration tests a fresh, migrated PostgreSQL database per test package,
// with one LOGIN user per service role (afs_owner, afs_app, afs_reconciler, afs_gateway,
// afs_billing). Tests skip when AFS_TEST_DATABASE_URL is unset.
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
var Roles = []string{"afs_owner", "afs_app", "afs_reconciler", "afs_gateway", "afs_billing"}

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
)

// Run runs the package's tests and drops the database afterwards.
func Run(m *testing.M) int {
	code := m.Run()
	if shared != nil {
		if err := shared.drop(context.Background()); err != nil {
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
	once.Do(func() { shared, setErr = create(admin) })
	if setErr != nil {
		t.Fatalf("pgtest: %v", setErr)
	}
	return shared
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

func create(admin string) (*DB, error) {
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
