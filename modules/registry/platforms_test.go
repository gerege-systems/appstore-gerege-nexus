package registry_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/gerege-systems/appstore-gerege-nexus/modules/registry"
	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/catalog"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The credential is read from the Authorization header and from nowhere else.
//
// A token on the query string would be in this service's access log, in any
// proxy's in front of it, and in whatever caches by URL between them — three
// copies of a secret in places nobody thinks of when it is time to rotate one.
// The parser is the only thing standing between that and a well-meaning client.
func TestOnlyABearerHeaderCarriesTheCredential(t *testing.T) {
	cases := []struct {
		header, want string
	}{
		{"Bearer abc123", "abc123"},
		{"bearer abc123", "abc123"},     // schemes are case-insensitive
		{"Bearer   abc123  ", "abc123"}, // and padded by hand often enough
		{"", ""},
		{"abc123", ""},       // no scheme is not a bearer token
		{"Basic abc123", ""}, // and neither is a different one
		{"Bearer", ""},
		{"Bearer ", ""},
	}
	for _, c := range cases {
		if got := registry.BearerTokenForTest(c.header); got != c.want {
			t.Errorf("bearerToken(%q) = %q, want %q", c.header, got, c.want)
		}
	}
}

// A token is stored as a digest, and the digest is the only thing that can be
// read back. What this asserts is the property that makes a stolen database
// useless for impersonating a hundred deployments.
func TestATokenIsStoredOnlyAsItsDigest(t *testing.T) {
	first, err := registry.NewTokenForTest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.NewTokenForTest()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two issued tokens were identical")
	}
	if len(first) < 40 {
		t.Fatalf("a token of %d characters is not 256 bits of randomness", len(first))
	}

	digest := registry.TokenDigestForTest(first)
	sum := sha256.Sum256([]byte(first))
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatal("the digest is not a plain SHA-256 of the token")
	}
	if digest == first {
		t.Fatal("the digest is the token")
	}
}

// Everything below needs a database. The App Store's schema is the core's
// migration 00038 plus this repository's own history, so a test that asserts on
// rows has to be pointed at a database where both have run:
//
//	APPSTORE_TEST_DATABASE_URL=postgres://... go test ./modules/registry
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("APPSTORE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("APPSTORE_TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The whole feature, end to end, at the level that matters: which apps come
// back for whom.
//
// Three answers from one registry — the anonymous one, the granted one, and the
// revoked one — because every way of getting this wrong produces two of the
// three correctly. A private app that leaks is a private app in the anonymous
// catalogue; a grant that does not work is a private app missing from the
// granted one; a revocation that does not is the same document coming back
// after the grant is gone.
func TestAPrivateAppReachesOnlyTheGrantedPlatform(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := registry.NewStore(pool)
	service := registry.NewCatalogService(store, goldenSigner(t))

	publisher, publicApp, privateApp := seedApps(t, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM store_apps WHERE publisher_id = $1`, publisher)
		_, _ = pool.Exec(ctx, `DELETE FROM store_publishers WHERE id = $1`, publisher)
	})

	// Seeding rows is not publishing, and only publishing moves the revision
	// counter the snapshot cache is keyed by — so without this the catalogue
	// served here is whatever a previous run of this test left behind, listing
	// apps that were deleted with it. The real publish path bumps the revision;
	// a fixture that writes rows directly has to say so itself.
	if _, err := store.DiscardSnapshots(ctx); err != nil {
		t.Fatalf("discard public snapshots: %v", err)
	}
	if err := store.DiscardPrivateSnapshots(ctx, ""); err != nil {
		t.Fatalf("discard private snapshots: %v", err)
	}

	platform, token, err := store.CreatePlatform(ctx, "Test Deployment", "https://test.example.mn", "", "")
	if err != nil {
		t.Fatalf("register a platform: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM store_platforms WHERE id = $1`, platform.ID) })

	ids := func(t *testing.T, tok string) map[string]bool {
		t.Helper()
		audience, err := store.AudienceFor(ctx, tok)
		if err != nil {
			t.Fatalf("audience: %v", err)
		}
		// Same reason as above: within one test nothing publishes, so every
		// read would otherwise be answered from the document the previous read
		// cached — including the one taken before a grant.
		if _, err := store.DiscardSnapshots(ctx); err != nil {
			t.Fatalf("discard public snapshots: %v", err)
		}
		if err := store.DiscardPrivateSnapshots(ctx, ""); err != nil {
			t.Fatalf("discard private snapshots: %v", err)
		}
		snapshot, err := service.Catalog(ctx, "stable", "1.0.0", audience)
		if err != nil {
			t.Fatalf("catalog: %v", err)
		}
		return appIDsIn(t, snapshot.Document)
	}

	// Nobody has been granted anything yet, and the credential changes nothing:
	// a platform with no grants is the anonymous audience under another name.
	for _, tok := range []string{"", token} {
		got := ids(t, tok)
		if !got[publicApp] {
			t.Errorf("the public app is missing from the catalogue (token %q)", tok)
		}
		if got[privateApp] {
			t.Errorf("a private app reached a platform nobody granted it to (token %q)", tok)
		}
	}

	if err := store.Grant(ctx, privateApp, platform.ID, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := store.DiscardPrivateSnapshots(ctx, platform.ID); err != nil {
		t.Fatalf("discard: %v", err)
	}

	granted := ids(t, token)
	if !granted[privateApp] {
		t.Error("the granted platform was not offered the private app")
	}
	if !granted[publicApp] {
		t.Error("granting a private app took the public one away")
	}
	// And the public catalogue is untouched by any of it.
	if ids(t, "")[privateApp] {
		t.Error("the private app leaked into the anonymous catalogue after a grant")
	}

	// Revocation has to take effect now, not at the next publication. The cache
	// is keyed by the registry's revision counter and a grant changes no
	// revision, so the handler discards the platform's cached document; this
	// asserts the store half of that.
	if err := store.Revoke(ctx, privateApp, platform.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := store.DiscardPrivateSnapshots(ctx, platform.ID); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if ids(t, token)[privateApp] {
		t.Error("a revoked app was still in the platform's catalogue")
	}

	// A revoked credential falls back to the public catalogue rather than to
	// nothing: revocation takes back what was granted, not the store.
	if err := store.Grant(ctx, privateApp, platform.ID, ""); err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	if err := store.SetPlatformEnabled(ctx, platform.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	disabled := ids(t, token)
	if disabled[privateApp] {
		t.Error("a disabled platform was still offered its private app")
	}
	if !disabled[publicApp] {
		t.Error("a disabled platform lost the public catalogue as well")
	}
}

// appIDsIn reads the app ids out of a signed document. The signature is not
// re-checked here — appcatalog's own tests do that, and what this file is about
// is which apps are in the list.
func appIDsIn(t *testing.T, document []byte) map[string]bool {
	t.Helper()
	var envelope struct {
		Apps []struct {
			ID string `json:"id"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(document, &envelope); err != nil {
		t.Fatalf("decode the catalogue document: %v", err)
	}
	ids := make(map[string]bool, len(envelope.Apps))
	for _, app := range envelope.Apps {
		ids[app.ID] = true
	}
	return ids
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return encoded
}

// randomSuffix keeps two runs of this test out of each other's rows.
func randomSuffix(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

// seedApps writes one public and one private published app and returns the
// publisher and the two app ids.
func seedApps(t *testing.T, pool *pgxpool.Pool) (publisher, public, private string) {
	t.Helper()
	ctx := context.Background()
	suffix := randomSuffix(t)
	public, private = "mn.test.public."+suffix, "mn.test.private."+suffix

	// A publisher belongs to a tenant — the store is an app on a platform, and
	// publishing is something an organisation does. The tenant is this test's
	// own, so the rows it writes cannot collide with a real one.
	var tenantID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ($1, 'Registry test') RETURNING id::text`,
		"registry-test-"+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID) })

	if err := pool.QueryRow(ctx,
		`INSERT INTO store_publishers (tenant_id, slug, name, verified)
		 VALUES ($1, $2, 'Test publisher', TRUE) RETURNING id::text`,
		tenantID, "test-"+suffix).Scan(&publisher); err != nil {
		t.Fatalf("seed publisher: %v", err)
	}

	for _, app := range []struct{ id, slug, visibility string }{
		{public, "public-" + suffix, "public"},
		{private, "private-" + suffix, "private"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO store_apps (id, publisher_id, slug, type, name, description, icon_url, category, visibility)
			 VALUES ($1, $2, $3, 'module', $3, '', '', 'Test', $4)`,
			app.id, publisher, app.slug, app.visibility); err != nil {
			t.Fatalf("seed app %s: %v", app.id, err)
		}
		// The manifest carries no visibility here on purpose: what the catalogue
		// builder filters on is store_apps.visibility, the column, and this test
		// is about that. The manifest field is the publisher's declaration on
		// the way *in* — see the submission path — and it reaches this column
		// rather than replacing it.
		manifest := catalog.Manifest{
			ID: app.id, Name: app.slug, Version: "1.0.0", Platform: ">=1.0.0",
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO store_app_versions (app_id, version, channel, min_platform, manifest, status, published_at)
			 VALUES ($1, '1.0.0', 'stable', '1.0.0', $2, 'published', NOW())`,
			app.id, mustJSON(t, manifest)); err != nil {
			t.Fatalf("seed version %s: %v", app.id, err)
		}
	}
	return publisher, public, private
}
