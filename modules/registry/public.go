/*
 * Gerege Nexus
 * Copyright (c) 2026 Gerege Systems Development Team, @craftzbay, Gemini AI & Claude AI
 * Distributed under the Apache 2.0 License.
 */

// The public face of the registry: the catalogue every instance reads, and the
// storefront's view of one app.
//
// None of it needs a session. That is not a relaxation — it is what a catalogue
// is. What makes the contents trustworthy is the signature over them, not who
// asked; an instance verifies the document against a pinned key before reading
// a single field of it, and would reject one served over a perfectly
// authenticated connection if the signature did not hold.

package registry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/nexus"

	"github.com/Masterminds/semver/v3"
	"github.com/go-chi/chi/v5"
)

// requireCatalogue answers 503 when this instance holds no signing key, and
// reports whether the caller may continue.
//
// An instance with no key is the ordinary case — only the App Store itself
// holds one — and it must not serve an unsigned catalogue. Every client
// verifies before reading, so an unsigned document would be rejected anyway;
// serving one would only teach a client that sometimes there is nothing to
// check.
func (m *Module) requireCatalogue(w http.ResponseWriter) bool {
	if m.catalog == nil {
		nexus.Error(w, http.StatusServiceUnavailable,
			"this instance does not publish an app catalogue")
		return false
	}
	return true
}

// handleCatalog is the endpoint every Nexus instance polls.
//
// It answers with bytes that were signed as a unit and stored that way; nothing
// here re-encodes them. The ETag is a hash of exactly those bytes, so a 304 is
// a statement about the document rather than about this request.
func (m *Module) handleCatalog(w http.ResponseWriter, r *http.Request) {
	if !m.requireCatalogue(w) {
		return
	}
	channel := r.URL.Query().Get("channel")
	platform := r.URL.Query().Get("platform")

	// A platform that is not a version is the caller's mistake. It used to
	// come back as a 500, which puts somebody's typo in the same alert as a
	// database that has gone away.
	if platform != "" {
		if _, err := semver.NewVersion(platform); err != nil {
			nexus.Error(w, http.StatusBadRequest, "platform must be a semver version, for example 1.0.0")
			return
		}
	}
	if channel != "" && channel != "stable" && channel != "beta" {
		nexus.Error(w, http.StatusBadRequest, `channel must be "stable" or "beta"`)
		return
	}

	// Who is asking, when they say. A deployment that has been granted a private
	// app presents the credential this registry issued it; everybody else — the
	// overwhelming majority, and every deployment before this existed — presents
	// nothing and gets exactly the catalogue they always did.
	//
	// An unrecognised or revoked credential is treated as no credential rather
	// than refused. Answering 401 would take the *public* catalogue away from a
	// deployment whose token was revoked, which is not what revocation means:
	// it takes back what was granted, not the store.
	audience, err := m.store.AudienceFor(r.Context(), bearerToken(r.Header.Get("Authorization")))
	if err != nil {
		slog.Error("could not resolve the catalog audience", "error", err)
		nexus.Error(w, http.StatusInternalServerError, "could not build the catalog")
		return
	}
	if !audience.Anonymous() {
		m.store.TouchPlatform(r.Context(), audience.PlatformID)
	}

	snapshot, err := m.catalog.Catalog(r.Context(), channel, platform, audience)
	if err != nil {
		slog.Error("could not build the catalog", "error", err, "channel", channel, "platform", platform)
		nexus.Error(w, http.StatusInternalServerError, "could not build the catalog")
		return
	}

	w.Header().Set("ETag", snapshot.ETag)
	// Private to the caller once it is answering per credential. A shared cache
	// holding one platform's catalogue and handing it to the next deployment
	// that asked would undo the whole of this, and it would look like a caching
	// win while it did so. Vary names the header the answer turns on, for
	// anything that caches by request rather than by directive.
	if audience.Anonymous() {
		w.Header().Set("Cache-Control", "public, max-age=60")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=60")
	}
	w.Header().Set("Vary", "Authorization")
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, snapshot.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(snapshot.Document); err != nil {
		slog.Warn("catalog response was cut short", "error", err)
	}
}

func (m *Module) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := m.store.ListApps(r.Context(), "")
	if err != nil {
		slog.Error("could not list apps", "error", err)
		nexus.Error(w, http.StatusInternalServerError, "could not list apps")
		return
	}
	locale := localeFrom(r)
	view := make([]map[string]any, 0, len(apps))
	for _, app := range apps {
		view = append(view, m.appView(r.Context(), app, locale))
	}
	nexus.JSON(w, http.StatusOK, view)
}

func (m *Module) handleGetApp(w http.ResponseWriter, r *http.Request) {
	app, err := m.store.AppBySlug(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, ErrNotFound) {
		nexus.Error(w, http.StatusNotFound, "app not found")
		return
	}
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load the app")
		return
	}

	versions, err := m.store.ListVersions(r.Context(), app.ID, true)
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load versions")
		return
	}

	view := m.appView(r.Context(), *app, localeFrom(r))
	view["versions"] = versions
	if len(versions) > 0 {
		view["manifest"] = versions[0].Manifest
	}
	if external, err := m.store.ExternalRegistration(r.Context(), app.ID); err == nil {
		view["external"] = external
	}
	nexus.JSON(w, http.StatusOK, view)
}

func (m *Module) handleListVersions(w http.ResponseWriter, r *http.Request) {
	app, err := m.store.AppBySlug(r.Context(), chi.URLParam(r, "slug"))
	if errors.Is(err, ErrNotFound) {
		nexus.Error(w, http.StatusNotFound, "app not found")
		return
	}
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load the app")
		return
	}
	versions, err := m.store.ListVersions(r.Context(), app.ID, true)
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load versions")
		return
	}
	nexus.JSON(w, http.StatusOK, versions)
}

// handleChronicle serves one app's published history.
func (m *Module) handleChronicle(w http.ResponseWriter, r *http.Request) {
	app, err := m.store.AppBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		nexus.Error(w, http.StatusNotFound, "app not found")
		return
	}
	entries, err := m.store.Chronicle(r.Context(), app.ID)
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load the chronicle")
		return
	}
	nexus.JSON(w, http.StatusOK, map[string]any{
		"app_id": app.ID, "slug": app.Slug, "entries": entries,
	})
}

func (m *Module) handleKeys(w http.ResponseWriter, _ *http.Request) {
	if !m.requireCatalogue(w) {
		return
	}
	// The keys a catalogue may be signed with, so an instance can follow a
	// rotation without a redeploy. Public for the same reason as
	// /.well-known/jwks.json: a client reads it in order to check a signature,
	// which is something it does before it trusts anything.
	nexus.JSON(w, http.StatusOK, map[string]any{
		"keys": []map[string]string{{
			"key_id": m.signer.KeyID,
			"alg":    "Ed25519",
			"public": m.signer.PublicKey(),
		}},
	})
}

// appView renders a catalogue entry in the caller's language.
//
// What a stranger may read: who wrote the app, who keeps it, its licence and
// where its source lives. What they may not: created_by and updated_by, which
// are the OIDC subjects of the people who touched the registration. Those exist
// for the review trail, not for the storefront — a public page that named the
// account which registered an app would be publishing an identifier nobody
// consented to publish.
func (m *Module) appView(ctx context.Context, app App, locale string) map[string]any {
	view := map[string]any{
		"id": app.ID, "slug": app.Slug, "type": app.Type, "name": app.Name,
		"description": app.Description, "icon_url": app.IconURL, "category": app.Category,
		"publisher": app.PublisherName, "latest_version": app.LatestVersion,
		"updated_at": app.UpdatedAt,
		"authors":    app.Authors, "maintainers": app.Maintainers,
		"repository": app.Repository, "homepage": app.Homepage, "license": app.License,
	}
	texts, err := m.store.AppTexts(ctx, app.ID)
	if err != nil {
		return view
	}
	view["translations"] = texts
	if text, ok := texts[locale]; ok {
		if text.Name != "" {
			view["name"] = text.Name
		}
		if text.Description != "" {
			view["description"] = text.Description
		}
		if text.Category != "" {
			view["category"] = text.Category
		}
	}
	return view
}

// localeFrom reads the language the caller asked for. The storefront passes it
// explicitly; a browser hitting the API directly gets Accept-Language.
func localeFrom(r *http.Request) string {
	if locale := r.URL.Query().Get("locale"); locale != "" {
		return locale
	}
	header := r.Header.Get("Accept-Language")
	if header == "" {
		return "en"
	}
	first := strings.SplitN(header, ",", 2)[0]
	return strings.ToLower(strings.TrimSpace(strings.SplitN(first, "-", 2)[0]))
}

// etagMatches implements the part of RFC 9110 §13.1.2 that matters here: a
// client may send several, and a proxy may have made ours weak.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// --- operator endpoints -----------------------------------------------------

// handleRegistryState reports what the registry is serving.
//
// Gated, unlike everything above it: the revision and the snapshot inventory
// say how this deployment is running rather than what it publishes, and a
// stranger has no business with either.
func (m *Module) handleRegistryState(w http.ResponseWriter, r *http.Request) {
	if !m.requireCatalogue(w) {
		return
	}
	revision, err := m.store.Revision(r.Context())
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not read the registry state")
		return
	}
	nexus.JSON(w, http.StatusOK, map[string]any{
		"revision":   revision,
		"key_id":     m.signer.KeyID,
		"public_key": m.signer.PublicKey(),
	})
}

// handleRebuildSnapshots discards the cached documents so the next request
// rebuilds and re-signs.
//
// It exists for the one case the revision counter cannot cover: the key
// changed. A snapshot is rebuilt when the revision moves, and rotating a
// signing key moves nothing — every cached document stays valid-looking and
// signed by a key the instances no longer accept.
func (m *Module) handleRebuildSnapshots(w http.ResponseWriter, r *http.Request) {
	if !m.requireCatalogue(w) {
		return
	}
	discarded, err := m.store.DiscardSnapshots(r.Context())
	if err != nil {
		slog.Error("could not discard the catalogue snapshots", "error", err)
		nexus.Error(w, http.StatusInternalServerError, "could not discard the snapshots")
		return
	}
	nexus.JSON(w, http.StatusOK, map[string]any{"discarded": discarded})
}
