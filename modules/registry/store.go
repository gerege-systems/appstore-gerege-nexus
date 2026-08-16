/*
 * Gerege Nexus — App Store registry
 * Copyright (c) 2026 Gerege Systems Development Team, Gerege Nomadica Foundation
 * Distributed under the Apache 2.0 License.
 *
 * Package appstore is the registry that serves appstore.gerege.mn: the
 * published catalogue every Nexus instance pulls, and the publishing side that
 * fills it.
 *
 * It shares the platform's appcatalog types deliberately. The wire contract
 * between this service and a Nexus instance is defined by the client in
 * internal/platform/appcatalog/source.go, and the cheapest way to keep two
 * programs agreeing about a format is to have them use the same structs and the
 * same validator rather than two descriptions of one thing.
 */

package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/catalog"

	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/nexus"

	"github.com/jackc/pgx/v5"
)

// Version statuses. A version is submitted for review, then published; a
// published version can be yanked, which stops it being offered without
// pretending it never existed.
const (
	StatusDraft     = "draft"
	StatusInReview  = "in_review"
	StatusPublished = "published"
	StatusRejected  = "rejected"
	StatusYanked    = "yanked"
)

// ErrNotFound is returned when an app, publisher or version does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when something already exists that may not be
// replaced — a version number that has been used, a slug somebody else holds.
var ErrConflict = errors.New("already exists")

// Store is the registry's persistence.
type Store struct{ db nexus.DB }

func NewStore(db nexus.DB) *Store { return &Store{db: db} }

// DB exposes the pool for tests that assert on rows this package writes but no
// caller reads — the snapshot cache in particular, whose whole contract is that
// it stays small.
func (s *Store) DB() nexus.DB { return s.db }

// Publisher is an organisation that publishes apps.
// Publisher is a tenant's publishing identity.
//
// It used to carry the owner as four columns — a subject claim, an e-mail and
// a tenant recorded as loose text — because the registry ran outside the
// platform and could only describe who was acting. Here the organisation is a
// tenant, so the row points at one and everything else about who may act
// follows from that tenant's own memberships and roles.
type Publisher struct {
	ID string `json:"id"`
	// TenantID is not served. It identifies an organisation inside this
	// deployment, and a storefront naming it would be publishing an internal
	// identifier to strangers; the slug is the public handle.
	TenantID     string     `json:"-"`
	Slug         string     `json:"slug"`
	Name         string     `json:"name"`
	ContactEmail string     `json:"contact_email"`
	Verified     bool       `json:"verified"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// App is a catalogue entry, without its versions.
type App struct {
	ID          string    `json:"id"`
	PublisherID string    `json:"publisher_id"`
	Slug        string    `json:"slug"`
	Type        string    `json:"type"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IconURL     string    `json:"icon_url"`
	Category    string    `json:"category"`
	Visibility  string    `json:"visibility"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// PublisherName is filled by the queries the storefront uses: a catalogue
	// entry without a publisher is an app nobody stands behind.
	PublisherName string `json:"publisher_name,omitempty"`
	// LatestVersion is the published version on the stable channel, when there
	// is one. Empty means the app exists but has nothing published.
	LatestVersion string `json:"latest_version,omitempty"`

	// Who stands behind the app, hoisted out of the manifest so the storefront
	// can render a credit line without decoding one.
	Authors     []catalog.Person `json:"authors,omitempty"`
	Maintainers []catalog.Person `json:"maintainers,omitempty"`
	Repository  string           `json:"repository,omitempty"`
	Homepage    string           `json:"homepage,omitempty"`
	License     string           `json:"license,omitempty"`
	// The registration's own trail. Not served to the public storefront — see
	// the API layer, which selects what a stranger may read.
	CreatedBy string `json:"created_by,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

// ChronicleEntry is one published version as the public chronicle serves it:
// what changed, when, and who reviewed it in.
type ChronicleEntry struct {
	Version     string               `json:"version"`
	Channel     string               `json:"channel"`
	PublishedAt *time.Time           `json:"published_at,omitempty"`
	Notes       *catalog.ReleaseNote `json:"release_notes,omitempty"`
	Authors     []string             `json:"authors,omitempty"`
}

// Version is one published (or pending) release of an app.
type Version struct {
	ID          string                `json:"id"`
	AppID       string                `json:"app_id"`
	Version     string                `json:"version"`
	Channel     string                `json:"channel"`
	MinPlatform string                `json:"min_platform"`
	Manifest    catalog.Manifest      `json:"manifest"`
	Status      string                `json:"status"`
	SubmittedBy string                `json:"submitted_by,omitempty"`
	ReviewNote  string                `json:"review_note,omitempty"`
	PublishedAt *time.Time            `json:"published_at,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
	App         *App                  `json:"app,omitempty"`
	External    *ExternalRegistration `json:"external,omitempty"`
}

// ExternalRegistration is the queryable form of a manifest's external section.
type ExternalRegistration struct {
	AppID       string   `json:"app_id"`
	LaunchURL   string   `json:"launch_url"`
	SSOClientID string   `json:"sso_client_id"`
	Scopes      []string `json:"scopes"`
	Embed       string   `json:"embed"`
	HealthURL   string   `json:"health_url"`
	WebhookURL  string   `json:"webhook_url,omitempty"`
}

// Revision returns the number that changes whenever the published catalogue
// does. A snapshot built under an older one is stale.
func (s *Store) Revision(ctx context.Context) (int64, error) {
	var revision int64
	err := s.db.QueryRow(ctx, `SELECT revision FROM store_registry_state WHERE id`).Scan(&revision)
	return revision, err
}

// DiscardSnapshots throws away every cached document, so the next request
// rebuilds and re-signs.
//
// The revision counter covers a catalogue whose *contents* changed. It cannot
// cover a signing key that changed: rotating one moves no revision, and every
// cached document stays looking valid while being signed by a key the instances
// have stopped accepting. This is the way out of that, and it is why rotation
// is an operator action rather than a redeploy.
func (s *Store) DiscardSnapshots(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM store_catalog_snapshots`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// bumpRevision is called inside every transaction that changes what the
// catalogue would say.
func bumpRevision(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE store_registry_state SET revision = revision + 1 WHERE id`)
	return err
}

// --- store_publishers -------------------------------------------------------------

// PublisherByOwner returns the publisher a signed-in developer acts for.
// PublisherByTenant resolves the publishing profile an organisation acts under.
//
// One tenant, one publisher: the unique constraint says so, and it is what
// makes "may this caller submit for this app" a question about membership
// rather than about a separate account somebody has to remember.
func (s *Store) PublisherByTenant(ctx context.Context, tenantID string) (*Publisher, error) {
	return s.scanPublisher(s.db.QueryRow(ctx, publisherColumns+` WHERE tenant_id = $1`, tenantID))
}

// PublisherBySlug resolves a publisher by its handle. The CI path uses it: the
// pipeline is bound to one publisher by configuration rather than naming its
// own in the request.
func (s *Store) PublisherBySlug(ctx context.Context, slug string) (*Publisher, error) {
	return s.scanPublisher(s.db.QueryRow(ctx, publisherColumns+` WHERE slug = $1`, slug))
}

func (s *Store) PublisherByID(ctx context.Context, id string) (*Publisher, error) {
	return s.scanPublisher(s.db.QueryRow(ctx, publisherColumns+` WHERE id = $1`, id))
}

const publisherColumns = `SELECT id::text, tenant_id::text, slug, name, contact_email,
	verified, verified_at, created_at FROM store_publishers`

func (s *Store) scanPublisher(row pgx.Row) (*Publisher, error) {
	var p Publisher
	err := row.Scan(&p.ID, &p.TenantID, &p.Slug, &p.Name, &p.ContactEmail,
		&p.Verified, &p.VerifiedAt, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpsertPublisher records or edits the profile a tenant publishes under.
//
// Verification is not touched here. It is somebody else's decision about this
// organisation, and a publisher that could edit its own verified flag would be
// a badge worth nothing.
func (s *Store) UpsertPublisher(ctx context.Context, p *Publisher) (*Publisher, error) {
	var id string
	err := s.db.QueryRow(ctx,
		`INSERT INTO store_publishers (tenant_id, slug, name, contact_email)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id) DO UPDATE SET
		     slug = EXCLUDED.slug, name = EXCLUDED.name,
		     contact_email = EXCLUDED.contact_email, updated_at = NOW()
		 RETURNING id::text`,
		p.TenantID, p.Slug, p.Name, p.ContactEmail).Scan(&id)
	if isUniqueViolation(err) {
		// The slug is the other unique column, and it is the one a caller can
		// collide on: somebody else already publishes under that handle.
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return s.PublisherByID(ctx, id)
}

// ListPublishers is an administrative read.
func (s *Store) ListPublishers(ctx context.Context) ([]Publisher, error) {
	rows, err := s.db.Query(ctx, publisherColumns+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]Publisher, 0)
	for rows.Next() {
		var p Publisher
		if err := rows.Scan(&p.ID, &p.TenantID, &p.Slug, &p.Name, &p.ContactEmail,
			&p.Verified, &p.VerifiedAt, &p.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

// SetPublisherVerified is the administrator's decision that a publisher is who
// they claim to be.
func (s *Store) SetPublisherVerified(ctx context.Context, id string, verified bool, actorID string) error {
	// Who decided, and when, alongside the flag itself. A badge that says
	// "verified" and cannot say by whom is a badge nobody can question.
	tag, err := s.db.Exec(ctx,
		`UPDATE store_publishers
		    SET verified = $1,
		        verified_at = CASE WHEN $1 THEN NOW() END,
		        verified_by = CASE WHEN $1 THEN NULLIF($3,'')::uuid END,
		        updated_at = NOW()
		  WHERE id = $2`, verified, id, actorID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- apps -------------------------------------------------------------------

const appColumns = `SELECT a.id, a.publisher_id::text, a.slug, a.type, a.name, a.description,
	a.icon_url, a.category, a.visibility, a.created_at, a.updated_at, p.name,
	COALESCE((SELECT v.version FROM store_app_versions v
	           WHERE v.app_id = a.id AND v.status = 'published' AND v.channel = 'stable'
	           ORDER BY v.published_at DESC LIMIT 1), ''),
	a.authors, a.maintainers, a.repository, a.homepage, a.license,
	-- Nullable columns read into strings. An app registered before anybody was
	-- recorded, or imported from a registry that had no such column, carries
	-- NULL here — and a catalogue that will not list an app because nobody is
	-- named as having created it is worse than one that says nothing about who.
	COALESCE(a.created_by::text, ''), COALESCE(a.updated_by::text, '')
	FROM store_apps a JOIN store_publishers p ON p.id = a.publisher_id`

// scanAppInto reads one row of appColumns. Both the single-row and the list
// paths go through it: two copies of a thirteen-column scan is how the second
// one gets forgotten the next time a column is added.
func scanAppInto(row pgx.Row, a *App) error {
	return row.Scan(&a.ID, &a.PublisherID, &a.Slug, &a.Type, &a.Name, &a.Description,
		&a.IconURL, &a.Category, &a.Visibility, &a.CreatedAt, &a.UpdatedAt,
		&a.PublisherName, &a.LatestVersion,
		&a.Authors, &a.Maintainers, &a.Repository, &a.Homepage, &a.License,
		&a.CreatedBy, &a.UpdatedBy)
}

func scanApp(row pgx.Row) (*App, error) {
	var a App
	err := scanAppInto(row, &a)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) AppByID(ctx context.Context, id string) (*App, error) {
	return scanApp(s.db.QueryRow(ctx, appColumns+` WHERE a.id = $1`, id))
}

func (s *Store) AppBySlug(ctx context.Context, slug string) (*App, error) {
	return scanApp(s.db.QueryRow(ctx, appColumns+` WHERE a.slug = $1`, slug))
}

// ListApps returns catalogue entries. publisherID filters to one publisher's
// own apps (the developer console); empty returns everything public.
//
// The public listing shows only apps with something published. It used to ask
// for visibility alone, so an app appeared in the storefront the moment it was
// registered — before review, with whatever placeholder text the submission
// form had been filled with, and with an empty latest_version. The signed
// catalogue never had this problem: it joins to published versions, so an
// unreviewed app could not reach an instance. The storefront is the public
// face of the same data and is now held to the same rule.
//
// A publisher's own listing is deliberately not filtered: an app you have
// registered and not yet published is exactly what you came to the console to
// see.
func (s *Store) ListApps(ctx context.Context, publisherID string) ([]App, error) {
	query := appColumns + `
		 WHERE a.visibility = 'public'
		   AND EXISTS (SELECT 1 FROM store_app_versions v
		                WHERE v.app_id = a.id AND v.status = 'published')
		 ORDER BY a.name`
	args := []any{}
	if publisherID != "" {
		query = appColumns + ` WHERE a.publisher_id = $1 ORDER BY a.name`
		args = append(args, publisherID)
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]App, 0)
	for rows.Next() {
		var a App
		if err := scanAppInto(rows, &a); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// Chronicle returns an app's published versions, newest first, with the release
// note each one carried.
//
// Public: it is the history of what a published app has done, which is exactly
// what somebody deciding whether to install it wants. Unpublished and yanked
// versions are absent — a version nobody can install is not part of the record
// the storefront offers.
func (s *Store) Chronicle(ctx context.Context, appID string) ([]ChronicleEntry, error) {
	rows, err := s.db.Query(ctx,
		`SELECT version, channel, published_at, release_notes, authors
		   FROM store_app_versions
		  WHERE app_id = $1 AND status = 'published'
		  ORDER BY published_at DESC NULLS LAST, version DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ChronicleEntry, 0, 8)
	for rows.Next() {
		var entry ChronicleEntry
		var notes, authors []byte
		if err := rows.Scan(&entry.Version, &entry.Channel, &entry.PublishedAt, &notes, &authors); err != nil {
			return nil, err
		}
		// A version published before the chronicle existed carries no note. It
		// still belongs in the list: it shipped, and a history that hides it is
		// wrong in the direction that matters.
		if len(notes) > 0 {
			var note catalog.ReleaseNote
			if err := json.Unmarshal(notes, &note); err == nil {
				entry.Notes = &note
			}
		}
		if len(authors) > 0 {
			_ = json.Unmarshal(authors, &entry.Authors)
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// jsonOrNull marshals a value, or returns SQL NULL for a nil pointer.
func jsonOrNull(value any) any {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

// jsonOrDefault marshals a value, falling back to a literal when it is empty —
// used for the JSONB columns declared NOT NULL DEFAULT '[]'.
func jsonOrDefault(value any, fallback string) any {
	raw, err := json.Marshal(value)
	if err != nil || string(raw) == "null" {
		return fallback
	}
	return raw
}

// UpsertApp creates or updates a catalogue entry. The publisher may not be
// changed by an update: an app belongs to whoever registered it.
func (s *Store) UpsertApp(ctx context.Context, a *App, texts map[string]catalog.CatalogAppText) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ownerID string
	err = tx.QueryRow(ctx, `SELECT publisher_id::text FROM store_apps WHERE id = $1`, a.ID).Scan(&ownerID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// New app.
	case err != nil:
		return err
	case ownerID != a.PublisherID:
		return ErrConflict
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO store_apps (id, publisher_id, slug, type, name, description, icon_url,
		                         category, visibility, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
		 ON CONFLICT (id) DO UPDATE SET
		     slug = EXCLUDED.slug, type = EXCLUDED.type, name = EXCLUDED.name,
		     description = EXCLUDED.description, icon_url = EXCLUDED.icon_url,
		     category = EXCLUDED.category, visibility = EXCLUDED.visibility,
		     updated_at = NOW()`,
		a.ID, a.PublisherID, a.Slug, a.Type, a.Name, a.Description, a.IconURL,
		a.Category, a.Visibility); err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}

	for locale, text := range texts {
		if _, err := tx.Exec(ctx,
			`INSERT INTO store_app_texts (app_id, locale, name, description, category)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (app_id, locale) DO UPDATE SET
			     name = EXCLUDED.name, description = EXCLUDED.description,
			     category = EXCLUDED.category`,
			a.ID, locale, text.Name, text.Description, text.Category); err != nil {
			return err
		}
	}

	if err := bumpRevision(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AppTexts returns every translation held for an app.
func (s *Store) AppTexts(ctx context.Context, appID string) (map[string]catalog.CatalogAppText, error) {
	rows, err := s.db.Query(ctx,
		`SELECT locale, name, description, category FROM store_app_texts WHERE app_id = $1`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	texts := make(map[string]catalog.CatalogAppText)
	for rows.Next() {
		var locale string
		var text catalog.CatalogAppText
		if err := rows.Scan(&locale, &text.Name, &text.Description, &text.Category); err != nil {
			return nil, err
		}
		texts[locale] = text
	}
	return texts, rows.Err()
}

// --- versions ---------------------------------------------------------------

const versionColumns = `SELECT id::text, app_id, version, channel, min_platform, manifest,
	status, submitted_by, review_note, published_at, created_at FROM store_app_versions`

func scanVersion(row pgx.Row) (*Version, error) {
	var v Version
	var manifest []byte
	err := row.Scan(&v.ID, &v.AppID, &v.Version, &v.Channel, &v.MinPlatform, &manifest,
		&v.Status, &v.SubmittedBy, &v.ReviewNote, &v.PublishedAt, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(manifest, &v.Manifest); err != nil {
		return nil, fmt.Errorf("decode manifest of %s %s: %w", v.AppID, v.Version, err)
	}
	return &v, nil
}

func (s *Store) VersionByID(ctx context.Context, id string) (*Version, error) {
	return scanVersion(s.db.QueryRow(ctx, versionColumns+` WHERE id = $1`, id))
}

// ListVersions returns an app's releases, newest first.
func (s *Store) ListVersions(ctx context.Context, appID string, publishedOnly bool) ([]Version, error) {
	query := versionColumns + ` WHERE app_id = $1 ORDER BY created_at DESC`
	if publishedOnly {
		query = versionColumns + ` WHERE app_id = $1 AND status = 'published' ORDER BY published_at DESC`
	}
	return s.queryVersions(ctx, query, appID)
}

// ListReviewQueue returns everything waiting for a decision, oldest first —
// a queue is answered in the order people joined it.
func (s *Store) ListReviewQueue(ctx context.Context) ([]Version, error) {
	versions, err := s.queryVersions(ctx,
		versionColumns+` WHERE status = 'in_review' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	for i := range versions {
		if app, err := s.AppByID(ctx, versions[i].AppID); err == nil {
			versions[i].App = app
		}
	}
	return versions, nil
}

func (s *Store) queryVersions(ctx context.Context, query string, args ...any) ([]Version, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]Version, 0)
	for rows.Next() {
		var v Version
		var manifest []byte
		if err := rows.Scan(&v.ID, &v.AppID, &v.Version, &v.Channel, &v.MinPlatform, &manifest,
			&v.Status, &v.SubmittedBy, &v.ReviewNote, &v.PublishedAt, &v.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(manifest, &v.Manifest); err != nil {
			return nil, err
		}
		list = append(list, v)
	}
	return list, rows.Err()
}

// SubmitVersion records a new release for review.
//
// A version number is used once. Re-submitting under a number that already
// exists is refused rather than overwritten: instances cache by version, and a
// manifest that changes under a version they already hold is a change they
// would never see.
// ErrInvalidSubmission is a manifest the caller can fix. Its message is meant
// to be shown to them, which is why it carries the reason rather than a code.
var ErrInvalidSubmission = errors.New("invalid submission")

// SubmitManifest validates a manifest and puts it in the review queue.
//
// Validation lives here rather than in a handler so that every way in — the
// studio, the release pipeline, whatever comes next — is held to the same
// rules. Two copies of these checks would differ the first time one of them was
// edited, and the difference would be a manifest one path accepted and the
// platform later rejected on arrival at an instance, which is a support ticket
// rather than an error message.
func (s *Store) SubmitManifest(ctx context.Context, app *App, channel string,
	manifest catalog.Manifest, submittedBy string) (*Version, error) {

	if channel == "" {
		channel = "stable"
	}
	if channel != "stable" && channel != "beta" {
		return nil, fmt.Errorf(`%w: channel must be "stable" or "beta"`, ErrInvalidSubmission)
	}
	if manifest.ID != app.ID {
		return nil, fmt.Errorf("%w: the manifest id must match the app id", ErrInvalidSubmission)
	}
	if (manifest.Type == catalog.TypeExternal) != (app.Type == catalog.TypeExternal) {
		return nil, fmt.Errorf("%w: the manifest type must match the app type", ErrInvalidSubmission)
	}
	// Validated with an empty platform version: the app's own constraint is
	// checked for sanity without being checked against this instance's version.
	// A registry serves catalogues to instances older and newer than itself, and
	// refusing a manifest for a platform it does not happen to be running would
	// make the registry's own version a publishing rule.
	if err := catalog.ValidateManifest(manifest, ""); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidSubmission, err.Error())
	}

	version := &Version{
		AppID: app.ID, Version: manifest.Version, Channel: channel,
		MinPlatform: manifest.Platform, Manifest: manifest,
		Status: StatusInReview, SubmittedBy: submittedBy,
	}
	if version.MinPlatform == "" {
		version.MinPlatform = ">=0.1.0"
	}
	return s.SubmitVersion(ctx, version, ExternalFromManifest(app.ID, manifest))
}

// ExternalFromManifest builds the queryable external registration, or nil for a
// module app.
func ExternalFromManifest(appID string, m catalog.Manifest) *ExternalRegistration {
	if !m.IsExternal() || m.External == nil {
		return nil
	}
	embed := m.External.Embed
	if embed == "" {
		embed = "new_tab"
	}
	return &ExternalRegistration{
		AppID: appID, LaunchURL: m.External.LaunchURL,
		SSOClientID: m.External.SSOClientID, Scopes: m.External.Scopes,
		Embed: embed, HealthURL: m.External.HealthURL,
	}
}

func (s *Store) SubmitVersion(ctx context.Context, v *Version, external *ExternalRegistration) (*Version, error) {
	manifest, err := json.Marshal(v.Manifest)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO store_app_versions (app_id, version, channel, min_platform, manifest,
		                                 status, submitted_by, release_notes, authors)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id::text`,
		v.AppID, v.Version, v.Channel, v.MinPlatform, manifest, v.Status, v.SubmittedBy,
		// Copied out of the manifest rather than asked for separately: the
		// manifest is the record that travels and is signed, and a second field
		// the submitter could fill differently is a second version of the truth.
		jsonOrNull(v.Manifest.ReleaseNotes), jsonOrDefault(v.Manifest.Authors, "[]")).Scan(&id)
	if isUniqueViolation(err) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}

	if external != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO store_external_registrations (app_id, launch_url, sso_client_id, scopes, embed, health_url)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (app_id) DO UPDATE SET
			     launch_url = EXCLUDED.launch_url, sso_client_id = EXCLUDED.sso_client_id,
			     scopes = EXCLUDED.scopes, embed = EXCLUDED.embed, health_url = EXCLUDED.health_url`,
			v.AppID, external.LaunchURL, external.SSOClientID, external.Scopes,
			external.Embed, external.HealthURL); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO store_review_events (version_id, actor, action, note) VALUES ($1, $2, 'submitted', '')`,
		id, v.SubmittedBy); err != nil {
		return nil, err
	}

	// A submission changes nothing a Nexus instance can see, so the revision is
	// deliberately not bumped here — only a decision does that.
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.VersionByID(ctx, id)
}

// DecideVersion publishes, rejects or yanks a version.
//
// Publishing is the only moment the catalogue every instance pulls actually
// changes, which is why the revision is bumped inside the same transaction: a
// snapshot built a microsecond earlier must not be served afterwards.
func (s *Store) DecideVersion(ctx context.Context, versionID, action, actorID, actor, note string) error {
	var status string
	switch action {
	case "publish":
		status = StatusPublished
	case "reject":
		status = StatusRejected
	case "yank":
		status = StatusYanked
	default:
		return fmt.Errorf("unknown review action %q", action)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	publishedAt := "published_at"
	if status == StatusPublished {
		publishedAt = "NOW()"
	}
	tag, err := tx.Exec(ctx,
		`UPDATE store_app_versions
		    SET status = $1, review_note = $2, published_at = `+publishedAt+`
		  WHERE id = $3`, status, note, versionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	// Publishing is what makes a manifest the app's public description, so it
	// is where the app row takes the provenance from it. Doing this at
	// submission instead would let an unreviewed manifest rewrite the licence
	// and the author list of an app that is already published.
	if status == StatusPublished {
		if _, err := tx.Exec(ctx,
			`UPDATE store_apps a SET
			     authors     = COALESCE(v.manifest -> 'authors', '[]'::jsonb),
			     maintainers = COALESCE(v.manifest -> 'maintainers', '[]'::jsonb),
			     repository  = COALESCE(v.manifest ->> 'repository', ''),
			     homepage    = COALESCE(v.manifest ->> 'homepage', ''),
			     license     = COALESCE(v.manifest ->> 'license', ''),
			     updated_by  = NULLIF($2,'')::uuid,
			     updated_at  = NOW()
			   FROM store_app_versions v
			  WHERE v.id = $1 AND a.id = v.app_id`, versionID, actorID); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO store_review_events (version_id, actor_id, actor, action, note)
		 VALUES ($1, NULLIF($2,'')::uuid, $3, $4, $5)`,
		versionID, actorID, actor, action, note); err != nil {
		return err
	}
	if err := bumpRevision(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReviewHistory returns what has happened to a version.
func (s *Store) ReviewHistory(ctx context.Context, versionID string) ([]map[string]any, error) {
	rows, err := s.db.Query(ctx,
		`SELECT actor, action, note, created_at FROM store_review_events
		  WHERE version_id = $1 ORDER BY created_at`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	history := make([]map[string]any, 0)
	for rows.Next() {
		var actor, action, note string
		var at time.Time
		if err := rows.Scan(&actor, &action, &note, &at); err != nil {
			return nil, err
		}
		history = append(history, map[string]any{
			"actor": actor, "action": action, "note": note, "created_at": at,
		})
	}
	return history, rows.Err()
}

// ExternalRegistration returns an external app's registration, if it has one.
func (s *Store) ExternalRegistration(ctx context.Context, appID string) (*ExternalRegistration, error) {
	var e ExternalRegistration
	err := s.db.QueryRow(ctx,
		`SELECT app_id, launch_url, sso_client_id, scopes, embed, health_url, webhook_url
		   FROM store_external_registrations WHERE app_id = $1`, appID).
		Scan(&e.AppID, &e.LaunchURL, &e.SSOClientID, &e.Scopes, &e.Embed, &e.HealthURL, &e.WebhookURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// isUniqueViolation reports whether the database refused a duplicate. The
// driver's error is a string away from being useful; 23505 is the whole answer.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
