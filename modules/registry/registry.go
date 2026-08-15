/*
 * Gerege Nexus
 * Copyright (c) 2026 Gerege Systems Development Team, @craftzbay, Gemini AI & Claude AI
 * Distributed under the Apache 2.0 License.
 */

// Package appstore_registry serves the signed App Store catalogue every Gerege
// Nexus instance reads.
//
// It used to be a service of its own, because the App Store was split out of
// this repository. It is a module again for the reason the platform exists: an
// app store is an app. Running it here gives it the session handling, the
// tenant model, the roles, the audit trail and the seven languages it was
// otherwise reimplementing — and it makes the most convincing demonstration of
// the platform be the platform's own storefront.
//
// The instance that mounts this is an ordinary Nexus instance whose catalogue
// happens to list it. Every other instance carries the tables empty and never
// looks at them.
//
// What is unusual about this module, and deliberate: it mounts public routes.
// A catalogue is read by an instance that holds no session — that is the whole
// point of a catalogue — so /api/v1/registry/* is served without the tenant
// gate. RegisterRoutes is handed the root router precisely so a module can do
// this, and route_policy_test.go is what stops it happening by accident: every
// path here is named in publicRoutes with the reason it needs no session.
package registry

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/nexus"
	"github.com/go-chi/chi/v5"
)

// Module is the registry as the platform sees it.
type Module struct {
	db      nexus.DB
	store   *Store
	catalog *CatalogService
	// signer is nil when this deployment publishes no catalogue. That is the
	// ordinary case: only the instance that *is* the App Store holds the key.
	// The public endpoints answer 503 rather than pretending, because an
	// unsigned catalogue is one every instance would reject anyway — and a
	// registry that served one would be a registry teaching its clients to stop
	// checking.
	signer *Signer
}

// New builds the registry module and registers it.
//
// The signing key comes from the environment rather than from the catalogue,
// because it is a deployment secret and not a property of the app: two
// instances running the same version of this module, one of them the App Store,
// differ only in whether APPSTORE_SIGNING_KEY is set.
func New(p nexus.Platform) *Module {
	db := p.DB()
	m := &Module{db: p.DB(), store: NewStore(db)}

	if encoded := os.Getenv("APPSTORE_SIGNING_KEY"); encoded != "" {
		signer, err := NewSigner(envOr("APPSTORE_SIGNING_KEY_ID", "appstore-2026"), encoded)
		if err != nil {
			// Not fatal: an instance that cannot sign should still boot and
			// still serve everything else it carries. What it must not do is
			// serve a catalogue nobody can verify, which is why the endpoints
			// check for a signer rather than this constructor refusing.
			slog.Error("app store: the signing key was rejected; this instance will not serve a catalogue",
				"error", err)
		} else {
			m.signer = signer
			m.catalog = NewCatalogService(m.store, signer)
			slog.Info("app store: catalogue signing key loaded", "public_key", signer.PublicKey())
		}
	}

	nexus.Register(m)
	return m
}

func (m *Module) ID() string      { return "io.gerege.nexus.appstore_registry" }
func (m *Module) Name() string    { return "App Store Registry" }
func (m *Module) Version() string { return "1.0.0" }

func (m *Module) Dependencies() []nexus.Dependency { return nil }

// Permissions gates the parts of the registry that are not public.
//
// Reading the catalogue needs none — it is public. What needs a permission is
// operating the registry: rebuilding a snapshot, reading the state a stranger
// should not see. Publishing and review belong to the other two modules.
func (m *Module) Permissions() []nexus.PermissionDefinition {
	return []nexus.PermissionDefinition{
		{Code: "registry.read", Name: "Read the registry",
			Description: "See the registry's state and its published catalogue"},
		{Code: "registry.manage", Name: "Operate the registry",
			Description: "Rebuild the catalogue snapshot and manage registry state"},
	}
}

func (m *Module) Menus() []nexus.MenuDefinition {
	return []nexus.MenuDefinition{
		{
			ID: "appstore_registry", Label: "App Registry",
			Path: "/module/appstore/registry", Icon: "boxes", Order: 10,
			Labels: map[string]string{
				"mn": "Аппын бүртгэл", "ar": "سجل التطبيقات", "zh": "应用注册表",
				"fr": "Registre des applications", "ru": "Реестр приложений",
				"es": "Registro de aplicaciones",
			},
		},
	}
}

// RegisterRoutes mounts the public catalogue and the gated operator endpoints.
//
// The split is the whole design. Everything under /api/v1/registry is read by
// somebody with no session — an instance pulling a catalogue, a storefront
// rendering a page — and is mounted on the root router outside the gate. The
// operator endpoints go through the gate like any other module's.
func (m *Module) RegisterRoutes(r chi.Router, gate func(http.Handler) http.Handler) {
	// Public. Named in publicRoutes; see the package comment.
	r.Route("/api/v1/registry", func(pub chi.Router) {
		pub.Get("/catalog", m.handleCatalog)
		pub.Get("/apps", m.handleListApps)
		pub.Get("/apps/{slug}", m.handleGetApp)
		pub.Get("/apps/{slug}/versions", m.handleListVersions)
		pub.Get("/apps/{slug}/chronicle", m.handleChronicle)
	})
	// The keys a catalogue may be signed with. Public by the same reasoning as
	// /.well-known/jwks.json: a client reads it to check a signature, which is
	// a thing it does before it trusts anything.
	r.Get("/.well-known/appstore-keys.json", m.handleKeys)

	// Operator endpoints, gated like every other module's routes.
	r.Route("/api/v1/appstore/registry", func(ops chi.Router) {
		ops.Use(gate)
		ops.Get("/state", m.handleRegistryState)
		ops.Post("/rebuild", m.handleRebuildSnapshots)

		// Who this registry recognises, and what each of them may be offered
		// beyond the public catalogue. Gated like the rest of this group: the
		// catalogue these decisions shape is public, the decisions are not, and
		// the list of who holds what names every private arrangement the store
		// has — which is the thing a private app exists to keep off a page.
		ops.Get("/platforms", m.handleListPlatforms)
		ops.Post("/platforms", m.handleCreatePlatform)
		ops.Put("/platforms/{id}/access", m.handleSetPlatformEnabled)
		ops.Put("/platforms/{id}/apps/{appID}", m.handleGrant)
		ops.Delete("/platforms/{id}/apps/{appID}", m.handleRevoke)
	})
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
