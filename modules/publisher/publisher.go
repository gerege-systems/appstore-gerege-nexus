/*
 * Gerege Nexus
 * Copyright (c) 2026 Gerege Systems Development Team, Gerege Nomadica Foundation
 * Distributed under the Apache 2.0 License.
 */

// Package publisher_studio is where an organisation publishes apps.
//
// It replaces the developer console that ran at developer.gerege.mn as a
// separate Next.js application with its own OAuth2 client, its own
// backend-for-frontend, its own id_token verifier and its own idea of who a
// publisher is. All four existed because the registry ran outside the platform
// and had no way to know who was calling.
//
// Inside, none of them is needed. The caller is a signed-in member of a tenant;
// the tenant is the publisher; who may submit on its behalf is a role its own
// administrator grants. What was a separate product with an authentication
// stack is a module with two permissions.
package publisher

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/catalog"

	"github.com/gerege-systems/appstore-gerege-nexus/modules/registry"
	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/nexus"
	"github.com/go-chi/chi/v5"
)

// maxSubmission bounds a manifest. A megabyte is far more than any manifest
// needs and far less than a body worth holding in memory.
const maxSubmission = 1 << 20

type Module struct {
	db    nexus.DB
	store *registry.Store
}

func New(p nexus.Platform) *Module {
	db := p.DB()
	m := &Module{db: p.DB(), store: registry.NewStore(db)}
	nexus.Register(m)
	return m
}

func (m *Module) ID() string      { return "io.gerege.nexus.publisher_studio" }
func (m *Module) Name() string    { return "Publisher Studio" }
func (m *Module) Version() string { return "1.0.0" }

// Dependencies names the registry, because this module writes rows the registry
// serves and there is no version of this that is useful without it.
func (m *Module) Dependencies() []nexus.Dependency {
	return []nexus.Dependency{
		{ID: "io.gerege.nexus.appstore_registry", VersionConstraint: ">=1.0.0"},
	}
}

func (m *Module) Permissions() []nexus.PermissionDefinition {
	return []nexus.PermissionDefinition{
		{Code: "publisher.read", Name: "See published apps",
			Description: "View this organisation's publishing profile, apps and submissions"},
		{Code: "publisher.manage", Name: "Publish apps",
			Description: "Register apps and submit versions for review on this organisation's behalf"},
	}
}

func (m *Module) Menus() []nexus.MenuDefinition {
	return []nexus.MenuDefinition{
		{
			ID: "publisher_studio", Label: "Publisher Studio",
			Path: "/module/publisher", Icon: "upload", Order: 20,
			Labels: map[string]string{
				"mn": "Нийтлэгчийн студи", "ar": "استوديو الناشر", "zh": "发布者工作室",
				"fr": "Studio de l'éditeur", "ru": "Студия издателя",
				"es": "Estudio del editor",
			},
		},
	}
}

// RegisterRoutes mounts everything behind the app gate. Nothing here is public:
// a publisher's unpublished work is theirs.
func (m *Module) RegisterRoutes(r chi.Router, gate func(http.Handler) http.Handler) {
	r.Route("/api/v1/publisher", func(pub chi.Router) {
		pub.Use(gate)
		pub.Get("/", m.handleProfile)
		pub.Put("/", m.handleSaveProfile)
		pub.Get("/apps", m.handleListApps)
		pub.Post("/apps", m.handleUpsertApp)
		pub.Get("/apps/{slug}/versions", m.handleListVersions)
		pub.Post("/apps/{slug}/versions", m.handleSubmitVersion)
	})
}

// publisherFor resolves the profile the caller's organisation publishes under,
// answering 404 when it has none yet.
func (m *Module) publisherFor(w http.ResponseWriter, r *http.Request) (*registry.Publisher, string, bool) {
	tenantID, ok := nexus.RequireTenant(w, r)
	if !ok {
		return nil, "", false
	}
	publisher, err := m.store.PublisherByTenant(r.Context(), tenantID)
	if errors.Is(err, registry.ErrNotFound) {
		nexus.Error(w, http.StatusNotFound,
			"this organisation has no publishing profile yet")
		return nil, tenantID, false
	}
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load the publishing profile")
		return nil, tenantID, false
	}
	return publisher, tenantID, true
}

func (m *Module) handleProfile(w http.ResponseWriter, r *http.Request) {
	publisher, _, ok := m.publisherFor(w, r)
	if !ok {
		return
	}
	nexus.JSON(w, http.StatusOK, publisher)
}

// handleSaveProfile records or edits how this organisation appears in the
// store.
//
// The slug is the public handle and is taken as given rather than derived from
// the tenant's own slug: an organisation's internal name and the name it
// publishes under are different decisions, and a store is the more permanent of
// the two.
func (m *Module) handleSaveProfile(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := nexus.RequireTenant(w, r)
	if !ok {
		return
	}
	claims, err := nexus.UserFromContext(r.Context())
	if err != nil {
		nexus.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body struct {
		Slug         string `json:"slug"`
		Name         string `json:"name"`
		ContactEmail string `json:"contact_email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSubmission)).Decode(&body); err != nil {
		nexus.Error(w, http.StatusBadRequest, "malformed request body")
		return
	}
	body.Slug = strings.ToLower(strings.TrimSpace(body.Slug))
	body.Name = strings.TrimSpace(body.Name)
	if !catalog.IsValidSlug(body.Slug) {
		nexus.Error(w, http.StatusBadRequest,
			"the publisher handle must be a slug: lowercase letters, digits and dashes")
		return
	}
	if body.Name == "" {
		nexus.Error(w, http.StatusBadRequest, "a publisher name is required")
		return
	}

	saved, err := m.store.UpsertPublisher(r.Context(), &registry.Publisher{
		TenantID: tenantID, Slug: body.Slug, Name: body.Name,
		ContactEmail: strings.TrimSpace(body.ContactEmail),
	})
	if errors.Is(err, registry.ErrConflict) {
		nexus.Error(w, http.StatusConflict, "another organisation already publishes under that handle")
		return
	}
	if err != nil {
		slog.Error("could not save a publishing profile", "error", err, "tenant_id", tenantID)
		nexus.Error(w, http.StatusInternalServerError, "could not save the publishing profile")
		return
	}
	nexus.Audit(r.Context(), tenantID, claims.UserID, "publisher.profile_saved", "publisher",
		map[string]any{"slug": saved.Slug})
	nexus.JSON(w, http.StatusOK, saved)
}

func (m *Module) handleListApps(w http.ResponseWriter, r *http.Request) {
	publisher, _, ok := m.publisherFor(w, r)
	if !ok {
		return
	}
	// A publisher's own listing is deliberately unfiltered by publication: an
	// app you have registered and not yet published is what you came to see.
	apps, err := m.store.ListApps(r.Context(), publisher.ID)
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load your apps")
		return
	}
	nexus.JSON(w, http.StatusOK, apps)
}

// handleUpsertApp registers an app or edits its catalogue entry.
//
// The id is fixed at registration and never changes: it is what every
// installation on every instance is keyed by, so an app that could rename
// itself would be an app that could orphan its own installations.
func (m *Module) handleUpsertApp(w http.ResponseWriter, r *http.Request) {
	publisher, tenantID, ok := m.publisherFor(w, r)
	if !ok {
		return
	}
	claims, err := nexus.UserFromContext(r.Context())
	if err != nil {
		nexus.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body struct {
		ID          string                            `json:"id"`
		Slug        string                            `json:"slug"`
		Type        string                            `json:"type"`
		Name        string                            `json:"name"`
		Description string                            `json:"description"`
		IconURL     string                            `json:"icon_url"`
		Category    string                            `json:"category"`
		Visibility  string                            `json:"visibility"`
		Texts       map[string]catalog.CatalogAppText `json:"translations"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSubmission)).Decode(&body); err != nil {
		nexus.Error(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if body.ID == "" || !catalog.IsValidSlug(body.Slug) || strings.TrimSpace(body.Name) == "" {
		nexus.Error(w, http.StatusBadRequest, "an app needs an id, a valid slug and a name")
		return
	}
	if body.Type == "" {
		body.Type = catalog.TypeModule
	}
	if body.Type != catalog.TypeModule && body.Type != catalog.TypeExternal {
		nexus.Error(w, http.StatusBadRequest, `type must be "module" or "external"`)
		return
	}
	// Visibility decides which platforms are offered this app at all — public
	// to every one of them, private to the ones the registry names — so an
	// unrecognised value is refused rather than stored. Read as public it would
	// publish an app on a typo ("Private", "internal"); read as private it
	// would hide one for a reason nobody could see. Empty means public, which
	// is what a publisher who has not thought about it means.
	if body.Visibility == "" {
		body.Visibility = catalog.VisibilityPublic
	}
	if body.Visibility != catalog.VisibilityPublic && body.Visibility != catalog.VisibilityPrivate {
		nexus.Error(w, http.StatusBadRequest, `visibility must be "public" or "private"`)
		return
	}
	if body.Category == "" {
		body.Category = "General"
	}

	app := &registry.App{
		ID: body.ID, PublisherID: publisher.ID, Slug: body.Slug, Type: body.Type,
		Name: strings.TrimSpace(body.Name), Description: body.Description,
		IconURL: body.IconURL, Category: body.Category, Visibility: body.Visibility,
		CreatedBy: claims.UserID, UpdatedBy: claims.UserID,
	}
	if err := m.store.UpsertApp(r.Context(), app, body.Texts); err != nil {
		if errors.Is(err, registry.ErrConflict) {
			// Somebody else registered this id. 409 rather than 404: the caller
			// chose the id and is entitled to know it is taken.
			nexus.Error(w, http.StatusConflict, "that app id belongs to another publisher")
			return
		}
		slog.Error("could not save an app", "error", err, "app_id", body.ID)
		nexus.Error(w, http.StatusInternalServerError, "could not save the app")
		return
	}
	nexus.Audit(r.Context(), tenantID, claims.UserID, "publisher.app_saved", "store_app",
		map[string]any{"app_id": app.ID})
	nexus.JSON(w, http.StatusOK, app)
}

// ownApp resolves one of the caller's own apps, answering 404 for anybody
// else's — whether another publisher's app exists is not this caller's
// business.
func (m *Module) ownApp(w http.ResponseWriter, r *http.Request) (*registry.App, string, bool) {
	publisher, tenantID, ok := m.publisherFor(w, r)
	if !ok {
		return nil, "", false
	}
	app, err := m.store.AppBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil || app.PublisherID != publisher.ID {
		nexus.Error(w, http.StatusNotFound, "app not found")
		return nil, tenantID, false
	}
	return app, tenantID, true
}

func (m *Module) handleListVersions(w http.ResponseWriter, r *http.Request) {
	app, _, ok := m.ownApp(w, r)
	if !ok {
		return
	}
	versions, err := m.store.ListVersions(r.Context(), app.ID, false)
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load versions")
		return
	}
	nexus.JSON(w, http.StatusOK, versions)
}

// handleSubmitVersion puts a manifest in the review queue.
//
// It cannot publish. A publisher submitting and a reviewer publishing are two
// people on purpose, and the whole point of the queue is that the second is not
// the first.
func (m *Module) handleSubmitVersion(w http.ResponseWriter, r *http.Request) {
	app, tenantID, ok := m.ownApp(w, r)
	if !ok {
		return
	}
	claims, err := nexus.UserFromContext(r.Context())
	if err != nil {
		nexus.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var body struct {
		Channel  string           `json:"channel"`
		Manifest catalog.Manifest `json:"manifest"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSubmission)).Decode(&body); err != nil {
		nexus.Error(w, http.StatusBadRequest, "malformed request body")
		return
	}

	saved, err := m.store.SubmitManifest(r.Context(), app, body.Channel, body.Manifest, claims.Email)
	switch {
	case errors.Is(err, registry.ErrInvalidSubmission):
		nexus.Error(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, registry.ErrConflict):
		nexus.Error(w, http.StatusConflict,
			"that version already exists; a published version is immutable, so submit a new number")
		return
	case err != nil:
		slog.Error("could not submit a version", "error", err, "app_id", app.ID)
		nexus.Error(w, http.StatusInternalServerError, "could not submit the version")
		return
	}
	nexus.Audit(r.Context(), tenantID, claims.UserID, "publisher.version_submitted", "store_app_version",
		map[string]any{"app_id": app.ID, "version": saved.Version})
	nexus.JSON(w, http.StatusCreated, saved)
}
