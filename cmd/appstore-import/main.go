/*
 * Gerege Nexus
 * Copyright (c) 2026 Gerege Systems Development Team, @craftzbay, Gemini AI & Claude AI
 * Distributed under the Apache 2.0 License.
 *
 * appstore-import moves a standalone registry's data onto this platform.
 */
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// What this does.
//
// The App Store ran as a service with a database of its own. It is three
// modules now, and this carries what that database holds into the schema they
// read — see docs/APPSTORE_PHASE2_PLAN.md §3.4.
//
// The one real translation is the publisher. The old schema described who owned
// one with four loose columns: a subject claim, an e-mail, and a tenant
// recorded as text that referred to a tenant in a different database. Here a
// publisher *is* a tenant, so each publisher becomes:
//
//	a tenant, keeping its slug, so its storefront URLs do not move
//	a user, from the owner's e-mail, if that person has no account here yet
//	an admin membership, so somebody can immediately act for it
//
// Everything else is copied across as it stands.
//
// It is safe to run twice. Every write is conditional on what is already there,
// so an interrupted run is finished by running it again rather than by working
// out how far it got. Nothing is deleted and nothing in the source is touched:
// the old registry keeps serving until the cutover, and remains the rollback.
func main() {
	var (
		from = flag.String("from", os.Getenv("APPSTORE_SOURCE_URL"),
			"the standalone registry's database")
		to = flag.String("to", os.Getenv("DATABASE_URL"),
			"this platform's database")
		dryRun = flag.Bool("dry-run", true,
			"report what would be written and write nothing (the default)")
		timeout = flag.Duration("timeout", 5*time.Minute, "overall timeout")
	)
	flag.Parse()

	if strings.TrimSpace(*from) == "" || strings.TrimSpace(*to) == "" {
		fail("both -from and -to are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	source, err := pgxpool.New(ctx, *from)
	if err != nil {
		fail("connect to the source: %v", err)
	}
	defer source.Close()
	target, err := pgxpool.New(ctx, *to)
	if err != nil {
		fail("connect to the target: %v", err)
	}
	defer target.Close()

	if *dryRun {
		fmt.Println("DRY RUN — nothing will be written. Pass -dry-run=false to apply.")
	}
	report, err := run(ctx, source, target, *dryRun)
	if err != nil {
		fail("%v", err)
	}
	report.print(*dryRun)
}

type report struct {
	publishers, tenants, users, memberships  int
	apps, versions, texts, externals, events int
	skipped                                  []string
}

func (r report) print(dryRun bool) {
	verb := "imported"
	if dryRun {
		verb = "would import"
	}
	fmt.Printf("\n%s:\n", verb)
	fmt.Printf("  publishers   %4d  (tenants %d, users %d, memberships %d)\n",
		r.publishers, r.tenants, r.users, r.memberships)
	fmt.Printf("  apps         %4d\n", r.apps)
	fmt.Printf("  versions     %4d\n", r.versions)
	fmt.Printf("  translations %4d\n", r.texts)
	fmt.Printf("  external     %4d\n", r.externals)
	fmt.Printf("  review trail %4d\n", r.events)
	for _, note := range r.skipped {
		fmt.Printf("  · %s\n", note)
	}
	if dryRun {
		fmt.Println("\nNothing was written.")
	}
}

// run does the whole import in one transaction on the target.
//
// One transaction because a half-imported registry is worse than none: a
// catalogue carrying apps whose publisher did not arrive would be served, and
// served wrongly. A dry run rolls the same transaction back, so what it reports
// is what the real run would do rather than a guess at it.
func run(ctx context.Context, source, target *pgxpool.Pool, dryRun bool) (report, error) {
	var r report

	tx, err := target.Begin(ctx)
	if err != nil {
		return r, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// publisher id → the same id here. Keeping it means store_apps.publisher_id
	// carries over untouched and nothing has to be rewritten.
	publishers, err := source.Query(ctx,
		`SELECT id::text, slug, name, contact_email, verified,
		        COALESCE(owner_email, ''), COALESCE(owner_tenant_slug, '')
		   FROM publishers ORDER BY created_at`)
	if err != nil {
		return r, fmt.Errorf("read publishers: %w", err)
	}
	defer publishers.Close()

	type publisher struct {
		id, slug, name, email string
		verified              bool
		ownerEmail, ownerSlug string
	}
	var list []publisher
	for publishers.Next() {
		var p publisher
		if err := publishers.Scan(&p.id, &p.slug, &p.name, &p.email, &p.verified,
			&p.ownerEmail, &p.ownerSlug); err != nil {
			return r, fmt.Errorf("read a publisher: %w", err)
		}
		list = append(list, p)
	}
	if err := publishers.Err(); err != nil {
		return r, fmt.Errorf("read publishers: %w", err)
	}

	for _, p := range list {
		tenantID, made, err := ensureTenant(ctx, tx, p.slug, p.ownerSlug, p.name)
		if err != nil {
			return r, fmt.Errorf("publisher %s: %w", p.slug, err)
		}
		if made {
			r.tenants++
		}

		// The owner needs an account here to act for the tenant. Without an
		// e-mail there is nobody to make one for, and the publisher arrives
		// with no member — which is recoverable by an administrator and is
		// better than inventing a person.
		if p.ownerEmail != "" {
			userID, madeUser, err := ensureUser(ctx, tx, p.ownerEmail, p.name)
			if err != nil {
				return r, fmt.Errorf("publisher %s owner: %w", p.slug, err)
			}
			if madeUser {
				r.users++
			}
			made, err := ensureAdminMembership(ctx, tx, tenantID, userID)
			if err != nil {
				return r, fmt.Errorf("publisher %s membership: %w", p.slug, err)
			}
			if made {
				r.memberships++
			}
		} else {
			r.skipped = append(r.skipped,
				fmt.Sprintf("publisher %q has no owner e-mail; it arrives with no member", p.slug))
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO store_publishers (id, tenant_id, slug, name, contact_email, verified)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (id) DO UPDATE SET
			     tenant_id = EXCLUDED.tenant_id, slug = EXCLUDED.slug,
			     name = EXCLUDED.name, contact_email = EXCLUDED.contact_email,
			     verified = EXCLUDED.verified, updated_at = NOW()`,
			p.id, tenantID, p.slug, p.name, p.email, p.verified); err != nil {
			return r, fmt.Errorf("write publisher %s: %w", p.slug, err)
		}
		r.publishers++
	}

	// The rest is a straight copy: same columns, same ids, prefixed tables.
	for _, step := range []struct {
		what   string
		read   string
		write  string
		fields int
		count  *int
	}{
		{
			what: "apps",
			read: `SELECT id, publisher_id::text, slug, type, name, description, icon_url,
			              category, visibility, created_at, updated_at
			         FROM store_apps ORDER BY created_at`,
			write: `INSERT INTO store_apps (id, publisher_id, slug, type, name, description,
			                                icon_url, category, visibility, created_at, updated_at)
			        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			        ON CONFLICT (id) DO NOTHING`,
			fields: 11, count: &r.apps,
		},
		{
			what: "versions",
			read: `SELECT id::text, app_id, version, channel, min_platform, manifest,
			              status, submitted_by, review_note, published_at, created_at
			         FROM store_app_versions ORDER BY created_at`,
			write: `INSERT INTO store_app_versions (id, app_id, version, channel, min_platform,
			                                        manifest, status, submitted_by, review_note,
			                                        published_at, created_at)
			        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			        ON CONFLICT (id) DO NOTHING`,
			fields: 11, count: &r.versions,
		},
		{
			what: "translations",
			read: `SELECT app_id, locale, name, description, category FROM store_app_texts`,
			write: `INSERT INTO store_app_texts (app_id, locale, name, description, category)
			        VALUES ($1,$2,$3,$4,$5) ON CONFLICT (app_id, locale) DO NOTHING`,
			fields: 5, count: &r.texts,
		},
		{
			what: "external registrations",
			read: `SELECT app_id, launch_url, sso_client_id, scopes, embed, health_url, webhook_url
			         FROM external_registrations`,
			write: `INSERT INTO store_external_registrations (app_id, launch_url, sso_client_id,
			                                                  scopes, embed, health_url, webhook_url)
			        VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (app_id) DO NOTHING`,
			fields: 7, count: &r.externals,
		},
		{
			what: "review trail",
			read: `SELECT id::text, version_id::text, actor, action, note, created_at
			         FROM review_events ORDER BY created_at`,
			write: `INSERT INTO store_review_events (id, version_id, actor, action, note, created_at)
			        VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (id) DO NOTHING`,
			fields: 6, count: &r.events,
		},
	} {
		n, err := copyRows(ctx, source, tx, step.read, step.write, step.fields)
		if err != nil {
			return r, fmt.Errorf("copy %s: %w", step.what, err)
		}
		*step.count = n
	}

	// The catalogue is not copied. It is rebuilt here from the versions that
	// arrived, and re-signed with this deployment's own key — which is the
	// point of the byte comparison at cutover: two implementations, one
	// document. Copying the old snapshots would compare a thing with itself.
	if _, err := tx.Exec(ctx, `UPDATE store_registry_state SET revision = revision + 1 WHERE id`); err != nil {
		return r, fmt.Errorf("bump the revision: %w", err)
	}

	if dryRun {
		return r, nil // the deferred rollback is the whole of it
	}
	if err := tx.Commit(ctx); err != nil {
		return r, fmt.Errorf("commit: %w", err)
	}
	return r, nil
}

// ensureTenant finds or creates the tenant a publisher becomes.
//
// The publisher's slug is preferred over the owner tenant's, because it is the
// one that appears in storefront URLs and those should not move.
func ensureTenant(ctx context.Context, tx pgx.Tx, slug, ownerSlug, name string) (string, bool, error) {
	if slug == "" {
		slug = ownerSlug
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM tenants WHERE slug = $1`, slug).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO tenants (name, slug) VALUES ($1, $2) RETURNING id::text`,
		name, slug).Scan(&id); err != nil {
		return "", false, fmt.Errorf("create tenant %q: %w", slug, err)
	}
	return id, true, nil
}

// ensureUser finds or creates the person who owned a publisher.
//
// The account is created with no usable password. Whoever it belongs to signs
// in the way everybody else on this platform does — E-ID, or a reset they ask
// for — and an imported account with a password somebody could guess would be
// worse than no account.
func ensureUser(ctx context.Context, tx pgx.Tx, email, name string) (string, bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE lower(email) = $1`, email).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, name) VALUES ($1, '!', $2) RETURNING id::text`,
		email, name).Scan(&id); err != nil {
		return "", false, fmt.Errorf("create user %q: %w", email, err)
	}
	return id, true, nil
}

// ensureAdminMembership puts the owner in their tenant with the admin role, so
// somebody can act for the publisher the moment the import finishes.
func ensureAdminMembership(ctx context.Context, tx pgx.Tx, tenantID, userID string) (bool, error) {
	var membershipID string
	err := tx.QueryRow(ctx,
		`INSERT INTO memberships (tenant_id, user_id) VALUES ($1, $2)
		 ON CONFLICT (tenant_id, user_id) DO UPDATE SET tenant_id = EXCLUDED.tenant_id
		 RETURNING id::text`, tenantID, userID).Scan(&membershipID)
	if err != nil {
		return false, fmt.Errorf("membership: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO membership_roles (membership_id, role_id)
		 SELECT $1, r.id FROM roles r
		  WHERE r.tenant_id = $2 AND r.code = 'admin'
		 ON CONFLICT DO NOTHING`, membershipID, tenantID)
	if err != nil {
		return false, fmt.Errorf("grant admin: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// copyRows reads every row of one query and writes it through another.
func copyRows(ctx context.Context, source *pgxpool.Pool, tx pgx.Tx, read, write string, fields int) (int, error) {
	rows, err := source.Query(ctx, read)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	written := 0
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return written, err
		}
		if len(values) != fields {
			return written, fmt.Errorf("expected %d columns, got %d", fields, len(values))
		}
		if _, err := tx.Exec(ctx, write, values...); err != nil {
			return written, err
		}
		written++
	}
	return written, rows.Err()
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "appstore-import: "+format+"\n", args...)
	os.Exit(1)
}
