/*
 * Gerege Nexus — App Store registry
 * Copyright (c) 2026 Gerege Systems Development Team, Gerege Nomadica Foundation
 * Distributed under the Apache 2.0 License.
 */

package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/catalog"

	"github.com/Masterminds/semver/v3"
	"github.com/jackc/pgx/v5"
)

// Signer holds the key the registry signs catalogues with.
//
// Ed25519 rather than RSA: the signature is 64 bytes, verification is fast
// enough to do on every boot of every instance, and there is exactly one
// algorithm — nothing to negotiate and therefore nothing to downgrade.
type Signer struct {
	KeyID   string
	private ed25519.PrivateKey
}

// NewSigner builds a signer from a base64 private key (64 bytes, as produced by
// ed25519.GenerateKey, or a 32-byte seed).
func NewSigner(keyID, encoded string) (*Signer, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode signing key: %w", err)
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return &Signer{KeyID: keyID, private: ed25519.PrivateKey(raw)}, nil
	case ed25519.SeedSize:
		return &Signer{KeyID: keyID, private: ed25519.NewKeyFromSeed(raw)}, nil
	default:
		return nil, fmt.Errorf("signing key is %d bytes; expected %d (private) or %d (seed)",
			len(raw), ed25519.PrivateKeySize, ed25519.SeedSize)
	}
}

// PublicKey returns the base64 public half — the value pinned in each Nexus
// instance's APPSTORE_PUBLIC_KEY.
func (s *Signer) PublicKey() string {
	return base64.StdEncoding.EncodeToString(s.private.Public().(ed25519.PublicKey))
}

// Sign produces the signature the client verifies: over generated_at, a
// newline, and the raw bytes of the apps array.
//
// The message shape is the client's, not this service's — see
// appcatalog.signedMessage. generated_at is inside it so a captured older
// catalogue cannot be replayed at an instance as if it were current.
func (s *Signer) Sign(generatedAt string, apps []byte) string {
	message := make([]byte, 0, len(generatedAt)+1+len(apps))
	message = append(message, generatedAt...)
	message = append(message, '\n')
	message = append(message, apps...)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(s.private, message))
}

// Snapshot is a signed catalogue document, ready to serve verbatim.
type Snapshot struct {
	Revision    int64
	GeneratedAt time.Time
	ETag        string
	Document    []byte
}

// CatalogService builds, signs and caches catalogue documents.
type CatalogService struct {
	store  *Store
	signer *Signer
	// platformVersion is what an unspecified ?platform= is treated as. A client
	// that does not say which platform it is gets the catalogue for the oldest
	// one supported, which is the conservative direction: it may be offered
	// less, never more than it can run.
	defaultPlatform string
}

func NewCatalogService(store *Store, signer *Signer) *CatalogService {
	return &CatalogService{store: store, signer: signer, defaultPlatform: "1.0.0"}
}

// Catalog returns the signed document for a channel and platform version,
// building and storing it when the one on file is stale.
//
// The bytes are stored rather than rebuilt because the signature covers them.
// Rebuilding per request would work only for as long as Go's JSON encoder makes
// byte-identical output for the same input for ever, and the failure mode if it
// ever does not is silent: every instance in the field rejects the signature and
// simply stops receiving updates.
func (c *CatalogService) Catalog(ctx context.Context, channel, platform string, audience Audience) (*Snapshot, error) {
	if channel == "" {
		channel = "stable"
	}
	if platform == "" {
		platform = c.defaultPlatform
	}

	revision, err := c.store.Revision(ctx)
	if err != nil {
		return nil, err
	}

	held, err := c.loadSnapshot(ctx, channel, platform, audience)
	if err != nil {
		return nil, err
	}
	if held != nil && held.Revision == revision {
		return held, nil
	}

	built, err := c.build(ctx, channel, platform, revision, audience)
	if err != nil {
		return nil, err
	}
	if err := c.saveSnapshot(ctx, channel, platform, audience, built); err != nil {
		// A snapshot that could not be stored is still a correct answer; the
		// next request rebuilds it. Failing the request instead would make a
		// full disk look like a broken registry.
		slog.Warn("could not cache the signed catalog; it will be rebuilt per request",
			"error", err, "channel", channel, "platform", platform)
	}
	return built, nil
}

// loadSnapshot reads the cached document for this audience.
//
// Two tables, because they have two owners. The public catalogue's cache is the
// core's store_catalog_snapshots, created by migration 00038 and untouched by
// this feature; a platform's private catalogue goes in store_private_snapshots,
// which this repository's own migration history owns. Widening the core's table
// with a column only the App Store has any use for would put App Store schema
// back in a migration every deployment of the platform runs.
func (c *CatalogService) loadSnapshot(ctx context.Context, channel, platform string, audience Audience) (*Snapshot, error) {
	var s Snapshot
	query := `SELECT revision, generated_at, etag, document FROM store_catalog_snapshots
		  WHERE channel = $1 AND platform = $2`
	args := []any{channel, platform}
	if !audience.Anonymous() {
		query = `SELECT revision, generated_at, etag, document FROM store_private_snapshots
		  WHERE channel = $1 AND platform = $2 AND platform_id = $3`
		args = append(args, audience.PlatformID)
	}
	err := c.store.db.QueryRow(ctx, query, args...).
		Scan(&s.Revision, &s.GeneratedAt, &s.ETag, &s.Document)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// maxSnapshotsPerChannel bounds the cache the catalogue endpoint writes to.
//
// That endpoint is deliberately unauthenticated, and it keeps one row per
// platform version asked for — so anything that can send a request can also
// make this table grow, one semver at a time, for as long as it likes. The
// number of platform versions in the field is small; the number a stranger can
// invent is not. Beyond this many, the least recently built are dropped: they
// rebuild on demand, so the only cost of being wrong here is arithmetic.
const maxSnapshotsPerChannel = 64

func (c *CatalogService) saveSnapshot(ctx context.Context, channel, platform string, audience Audience, s *Snapshot) error {
	if !audience.Anonymous() {
		// Not pruned on a bound of its own: a row here can only be created by a
		// request carrying a credential this registry issued, so the number of
		// them is the number of platforms an operator has registered times the
		// versions they run — not something a stranger can inflate. That is the
		// difference from the public cache, whose bound exists precisely
		// because anyone can ask it for one more semver.
		_, err := c.store.db.Exec(ctx,
			`INSERT INTO store_private_snapshots
			     (platform_id, channel, platform, revision, generated_at, etag, document, built_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
			 ON CONFLICT (platform_id, channel, platform) DO UPDATE SET
			     revision = EXCLUDED.revision, generated_at = EXCLUDED.generated_at,
			     etag = EXCLUDED.etag, document = EXCLUDED.document, built_at = NOW()`,
			audience.PlatformID, channel, platform, s.Revision, s.GeneratedAt, s.ETag, s.Document)
		return err
	}
	defer func() {
		if err := c.pruneSnapshots(ctx, channel); err != nil {
			slog.Warn("could not prune the catalog snapshot cache", "error", err, "channel", channel)
		}
	}()
	_, err := c.store.db.Exec(ctx,
		`INSERT INTO store_catalog_snapshots (channel, platform, revision, generated_at, etag, document, built_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())
		 ON CONFLICT (channel, platform) DO UPDATE SET
		     revision = EXCLUDED.revision, generated_at = EXCLUDED.generated_at,
		     etag = EXCLUDED.etag, document = EXCLUDED.document, built_at = NOW()`,
		channel, platform, s.Revision, s.GeneratedAt, s.ETag, s.Document)
	return err
}

// pruneSnapshots keeps the cache to its bound, newest first.
func (c *CatalogService) pruneSnapshots(ctx context.Context, channel string) error {
	_, err := c.store.db.Exec(ctx,
		`DELETE FROM store_catalog_snapshots
		  WHERE channel = $1
		    AND platform NOT IN (
		        SELECT platform FROM store_catalog_snapshots
		         WHERE channel = $1
		         ORDER BY built_at DESC
		         LIMIT $2)`, channel, maxSnapshotsPerChannel)
	return err
}

// signedDocument mirrors what the client parses. Field order here is the order
// on the wire; apps is raw because it is what the signature covers.
type signedDocument struct {
	GeneratedAt string          `json:"generated_at"`
	KeyID       string          `json:"key_id"`
	Apps        json.RawMessage `json:"apps"`
	Signature   string          `json:"signature"`
}

// build assembles every published app the given platform can run — the public
// ones, plus whatever this audience has been granted.
func (c *CatalogService) build(ctx context.Context, channel, platform string, revision int64, audience Audience) (*Snapshot, error) {
	platformVersion, err := semver.NewVersion(platform)
	if err != nil {
		return nil, fmt.Errorf("invalid platform version %q: %w", platform, err)
	}

	// Every published version, not just the newest: which one an instance is
	// offered depends on the platform it is running, and that is a semver
	// constraint SQL cannot evaluate. Choosing in SQL and filtering afterwards
	// was the first shape of this query, and it made an app disappear from the
	// catalogue of every older instance the moment a newer one was published —
	// the app they already had installed simply stopped being listed.
	rows, err := c.store.db.Query(ctx,
		`SELECT a.id, a.slug, a.name, a.description, a.icon_url, a.category, a.visibility,
		        v.version, v.min_platform, v.manifest, v.published_at
		   FROM store_apps a
		   JOIN store_app_versions v ON v.app_id = a.id
		  WHERE (a.visibility = 'public' OR a.id = ANY($2))
		    AND v.channel = $1 AND v.status = 'published'
		  ORDER BY a.id, v.published_at`, channel, audience.AppIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// generated_at is the newest publication in the document rather than the
	// wall clock. Two requests that would produce the same catalogue then
	// produce the same bytes, which is what makes the ETag mean "unchanged"
	// instead of "asked again".
	var generatedAt time.Time
	chosen := make(map[string]catalog.CatalogApp)
	order := make([]string, 0)
	for rows.Next() {
		var app catalog.CatalogApp
		var minPlatform string
		var manifest []byte
		var publishedAt *time.Time
		if err := rows.Scan(&app.ID, &app.Slug, &app.Name, &app.Description, &app.IconURL,
			&app.Category, &app.Visibility, &app.Version, &minPlatform, &manifest, &publishedAt); err != nil {
			return nil, err
		}

		// A version that needs a newer platform than the caller is running is
		// skipped rather than offered and refused: an instance that cannot run
		// a release should be offered the newest one it can.
		if !platformAllows(minPlatform, platformVersion) {
			continue
		}
		if err := json.Unmarshal(manifest, &app.Manifest); err != nil {
			return nil, fmt.Errorf("decode manifest of %s: %w", app.ID, err)
		}

		held, seen := chosen[app.ID]
		if !seen {
			order = append(order, app.ID)
		}
		if seen && !catalog.IsNewerVersion(app.Version, held.Version) {
			continue
		}
		chosen[app.ID] = app
		if publishedAt != nil && publishedAt.After(generatedAt) {
			generatedAt = *publishedAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	apps := make([]catalog.CatalogApp, 0, len(order))
	for _, id := range order {
		apps = append(apps, chosen[id])
	}

	for i := range apps {
		texts, err := c.store.AppTexts(ctx, apps[i].ID)
		if err != nil {
			return nil, err
		}
		if len(texts) > 0 {
			apps[i].Translations = texts
		}
	}

	// Validated before it is signed. A Nexus instance runs the same validation
	// on arrival and discards the whole document if it fails, so publishing
	// something invalid would not break instances — it would silently stop them
	// receiving updates, which is worse because nobody would notice.
	if err := catalog.ValidateCatalog(apps, platform); err != nil {
		return nil, fmt.Errorf("refusing to sign an invalid catalog: %w", err)
	}

	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })

	document, generatedAt, err := SignDocument(c.signer, generatedAt, apps)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(document)
	return &Snapshot{
		Revision:    revision,
		GeneratedAt: generatedAt,
		ETag:        `"sha256:` + hex.EncodeToString(sum[:]) + `"`,
		Document:    document,
	}, nil
}

// SignDocument assembles the document a Nexus instance parses.
//
// One function, used by the endpoint, by the offline signing tool and by the
// test that feeds the result to the real client: the format is agreed in one
// place or it is not agreed at all. The apps array is marshalled once and those
// bytes are both signed and embedded — encoding it twice would eventually
// produce two different byte strings and a signature nobody can verify.
func SignDocument(signer *Signer, generatedAt time.Time, apps []catalog.CatalogApp) ([]byte, time.Time, error) {
	appsJSON, err := json.Marshal(apps)
	if err != nil {
		return nil, generatedAt, err
	}
	if generatedAt.IsZero() {
		// An empty catalogue still needs a stable timestamp inside the
		// signature; the epoch is the one value that cannot drift per request.
		generatedAt = time.Unix(0, 0).UTC()
	}
	generatedAt = generatedAt.UTC()
	stamp := generatedAt.Format(time.RFC3339)

	document, err := json.Marshal(signedDocument{
		GeneratedAt: stamp,
		KeyID:       signer.KeyID,
		Apps:        appsJSON,
		Signature:   signer.Sign(stamp, appsJSON),
	})
	if err != nil {
		return nil, generatedAt, err
	}
	return document, generatedAt, nil
}

// platformAllows reports whether a platform version satisfies a version's
// constraint. An unparseable constraint excludes the version: publishing
// something nobody can install is a mistake that should be visible in the
// storefront, not one that widens what instances accept.
func platformAllows(constraint string, platform *semver.Version) bool {
	if constraint == "" {
		return true
	}
	parsed, err := semver.NewConstraint(constraint)
	if err != nil {
		return false
	}
	return parsed.Check(platform)
}
