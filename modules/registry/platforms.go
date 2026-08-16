/*
 * Gerege Nexus — App Store registry
 * Copyright (c) 2026 Gerege Systems Development Team, Gerege Nomadica Foundation
 * Distributed under the Apache 2.0 License.
 */

// Platforms: the deployments this registry can recognise, and what each of them
// may be offered beyond the public catalogue.
//
// The catalogue endpoint is public because a catalogue is public — an instance
// pulling one holds no session, and what makes the contents trustworthy is the
// signature over them rather than who asked. That answers "what may everybody
// see" completely, and it is the whole answer for every deployment that has
// been granted nothing.
//
// A private app makes the other question askable: not "is this document
// genuine" but "who is entitled to read it". A signature cannot answer that,
// so a deployment that has been granted a private app presents a credential and
// gets the catalogue it is entitled to. Nothing else about the endpoint
// changes: the answer is still signed, still cached, still verified by the
// client against the same pinned key.

package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Platform is one deployment in the field.
type Platform struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Origin     string     `json:"origin,omitempty"`
	Note       string     `json:"note,omitempty"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`

	// GrantedApps is filled by the listing the console reads. It is what an
	// operator is actually deciding about.
	GrantedApps []string `json:"granted_apps,omitempty"`
}

// Enabled reports whether this platform's credential is still accepted.
func (p Platform) Enabled() bool { return p.DisabledAt == nil }

// Audience is who a catalogue is being built for.
//
// The zero value is the anonymous one — every deployment that presents no
// credential, which is nearly all of them — and it is the only audience the
// public catalogue has ever had.
type Audience struct {
	// PlatformID is empty for an anonymous request.
	PlatformID string
	// AppIDs are the private apps this platform has been granted. Never used
	// as "only these": the catalogue is the public set *plus* this.
	AppIDs []string
}

// Anonymous reports whether this is a request with no credential.
func (a Audience) Anonymous() bool { return a.PlatformID == "" }

// tokenDigest is how a credential is stored and compared.
//
// SHA-256 with no salt and no work factor, deliberately. This is not a
// password: it is 256 bits from crypto/rand, so there is no dictionary to run
// and nothing a slow hash would buy — while a slow hash on the catalogue
// endpoint would be a cost every deployment in the field pays every hour.
// What the digest is for is that a stolen database does not hand over working
// credentials for a hundred deployments.
func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newToken mints a credential: 32 bytes of randomness, URL-safe.
//
// Returned once. The registry keeps only the digest, so an operator who loses
// it issues a new one rather than being shown the old.
func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// bearerToken pulls the credential out of an Authorization header.
//
// Only the Bearer scheme, and only from the header. A token on the query string
// would be in this service's access logs, in any proxy's in front of it, and in
// the ETag cache key of anything between — which is three copies of a secret in
// places nobody thinks to look when it is time to rotate one.
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// PlatformByToken resolves a credential to the platform that holds it.
//
// A token that matches nothing, or matches a disabled platform, is not an
// error: it is an anonymous request. The endpoint answers with the public
// catalogue rather than a 401, because the alternative is a deployment whose
// token was revoked losing its store entirely — including the public apps it
// was running before anybody granted it anything. Revocation should take away
// what was granted, not everything.
func (s *Store) PlatformByToken(ctx context.Context, token string) (*Platform, error) {
	if token == "" {
		return nil, nil
	}
	var p Platform
	err := s.db.QueryRow(ctx,
		`SELECT id::text, name, origin, note, disabled_at, last_seen_at, created_at
		   FROM store_platforms WHERE token_digest = $1`, tokenDigest(token)).
		Scan(&p.ID, &p.Name, &p.Origin, &p.Note, &p.DisabledAt, &p.LastSeenAt, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !p.Enabled() {
		return nil, nil
	}
	return &p, nil
}

// GrantedAppIDs lists the private apps this platform may be offered.
//
// Sorted, because the result reaches the catalogue query and the catalogue's
// bytes are signed: two requests that should produce the same document must
// produce the same bytes, and an unordered set from the database is not a
// promise of that.
func (s *Store) GrantedAppIDs(ctx context.Context, platformID string) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT app_id FROM store_app_grants WHERE platform_id = $1 ORDER BY app_id`, platformID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AudienceFor resolves a credential into what the catalogue builder needs.
func (s *Store) AudienceFor(ctx context.Context, token string) (Audience, error) {
	platform, err := s.PlatformByToken(ctx, token)
	if err != nil || platform == nil {
		return Audience{}, err
	}
	granted, err := s.GrantedAppIDs(ctx, platform.ID)
	if err != nil {
		return Audience{}, err
	}
	// A recognised platform with no grants is the anonymous audience in
	// everything but name, and saying so here keeps it on the public snapshot
	// cache instead of building it a private one of its own.
	if len(granted) == 0 {
		return Audience{}, nil
	}
	return Audience{PlatformID: platform.ID, AppIDs: granted}, nil
}

// TouchPlatform records that a credential was accepted.
//
// Best effort by design: this runs on the hot path of an endpoint every
// deployment polls, and a write failure here must not cost anybody their
// catalogue. What it buys is the one thing an operator cannot otherwise find
// out — whether a token they issued has ever been used.
func (s *Store) TouchPlatform(ctx context.Context, platformID string) {
	_, _ = s.db.Exec(ctx,
		`UPDATE store_platforms SET last_seen_at = NOW() WHERE id = $1`, platformID)
}

// CreatePlatform registers a deployment and returns its one-time token.
func (s *Store) CreatePlatform(ctx context.Context, name, origin, note, actorID string) (*Platform, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, "", fmt.Errorf("a platform needs a name")
	}
	token, err := newToken()
	if err != nil {
		return nil, "", err
	}
	var p Platform
	err = s.db.QueryRow(ctx,
		`INSERT INTO store_platforms (name, origin, note, token_digest, created_by)
		 VALUES ($1, $2, $3, $4, NULLIF($5,'')::uuid)
		 RETURNING id::text, name, origin, note, disabled_at, last_seen_at, created_at`,
		name, strings.TrimSpace(origin), strings.TrimSpace(note), tokenDigest(token), actorID).
		Scan(&p.ID, &p.Name, &p.Origin, &p.Note, &p.DisabledAt, &p.LastSeenAt, &p.CreatedAt)
	if err != nil {
		return nil, "", err
	}
	return &p, token, nil
}

// ListPlatforms returns every registered deployment with what it may see.
func (s *Store) ListPlatforms(ctx context.Context) ([]Platform, error) {
	rows, err := s.db.Query(ctx,
		`SELECT p.id::text, p.name, p.origin, p.note, p.disabled_at, p.last_seen_at, p.created_at,
		        COALESCE(ARRAY_AGG(g.app_id ORDER BY g.app_id) FILTER (WHERE g.app_id IS NOT NULL), '{}')
		   FROM store_platforms p
		   LEFT JOIN store_app_grants g ON g.platform_id = p.id
		  GROUP BY p.id
		  ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]Platform, 0)
	for rows.Next() {
		var p Platform
		if err := rows.Scan(&p.ID, &p.Name, &p.Origin, &p.Note, &p.DisabledAt,
			&p.LastSeenAt, &p.CreatedAt, &p.GrantedApps); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

// SetPlatformEnabled revokes or restores a credential.
func (s *Store) SetPlatformEnabled(ctx context.Context, platformID string, enabled bool) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE store_platforms SET disabled_at = CASE WHEN $2 THEN NULL ELSE NOW() END
		  WHERE id = $1`, platformID, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Grant lets a platform see a private app. Granting twice is not an error: an
// operator pressing a button again means the same thing they meant the first
// time.
func (s *Store) Grant(ctx context.Context, appID, platformID, actorID string) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO store_app_grants (app_id, platform_id, granted_by)
		 VALUES ($1, $2, NULLIF($3,'')::uuid)
		 ON CONFLICT (app_id, platform_id) DO NOTHING`, appID, platformID, actorID)
	return err
}

// Revoke takes it away. The platform's cached private catalogue goes with it —
// see discardPrivateSnapshots — because a revocation that leaves a signed
// document on disk is a revocation that takes effect at the next publish.
func (s *Store) Revoke(ctx context.Context, appID, platformID string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM store_app_grants WHERE app_id = $1 AND platform_id = $2`, appID, platformID)
	return err
}

// DiscardPrivateSnapshots drops the cached catalogues of one platform, or of
// every platform when the id is empty.
func (s *Store) DiscardPrivateSnapshots(ctx context.Context, platformID string) error {
	if platformID == "" {
		_, err := s.db.Exec(ctx, `DELETE FROM store_private_snapshots`)
		return err
	}
	_, err := s.db.Exec(ctx, `DELETE FROM store_private_snapshots WHERE platform_id = $1`, platformID)
	return err
}
