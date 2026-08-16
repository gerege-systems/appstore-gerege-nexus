/*
 * Gerege Nexus
 * Copyright (c) 2026 Gerege Systems Development Team, Gerege Nomadica Foundation
 * Distributed under the Apache 2.0 License.
 */

// Package store_review is the queue a version waits in, and the decision that
// ends the wait.
//
// It is a module of its own rather than a corner of publisher_studio because
// the two are held by different people on purpose. A publisher submits; a
// reviewer publishes. Putting both behind one permission would make the queue
// decorative — anybody who could submit could approve their own submission —
// and separating them is the entire reason a queue exists.
//
// Who reviews used to be named by an environment variable, APPSTORE_ADMIN_EMAILS,
// because the registry ran outside the platform and had no roles to draw on.
// Here it is a permission, granted to a role, assigned to a membership, and
// changing it is an ordinary administrative act with an audit trail — rather
// than a redeploy.
package review

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gerege-systems/appstore-gerege-nexus/modules/registry"
	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/nexus"
	"github.com/go-chi/chi/v5"
)

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

func (m *Module) ID() string      { return "io.gerege.nexus.store_review" }
func (m *Module) Name() string    { return "Store Review" }
func (m *Module) Version() string { return "1.0.0" }

func (m *Module) Dependencies() []nexus.Dependency {
	return []nexus.Dependency{
		{ID: "io.gerege.nexus.appstore_registry", VersionConstraint: ">=1.0.0"},
	}
}

// Permissions separates seeing the queue from deciding on it.
//
// Reading is the weaker of the two and is genuinely useful on its own: somebody
// triaging submissions, or answering a publisher asking where theirs has got
// to, needs to see the queue and has no business publishing from it.
func (m *Module) Permissions() []nexus.PermissionDefinition {
	return []nexus.PermissionDefinition{
		{Code: "review.read", Name: "See the review queue",
			Description: "View submitted versions and their review history"},
		{Code: "review.decide", Name: "Publish and reject",
			Description: "Publish, reject or withdraw a submitted version, and verify a publisher"},
	}
}

func (m *Module) Menus() []nexus.MenuDefinition {
	return []nexus.MenuDefinition{
		{
			ID: "store_review", Label: "Review Queue",
			Path: "/module/store-review", Icon: "list-checks", Order: 30,
			Labels: map[string]string{
				"mn": "Хяналтын дараалал", "ar": "قائمة المراجعة", "zh": "审核队列",
				"fr": "File de revue", "ru": "Очередь проверки",
				"es": "Cola de revisión",
			},
		},
	}
}

func (m *Module) RegisterRoutes(r chi.Router, gate func(http.Handler) http.Handler) {
	r.Route("/api/v1/store-review", func(rev chi.Router) {
		rev.Use(gate)
		rev.Get("/queue", m.handleQueue)
		rev.Get("/versions/{id}/history", m.handleHistory)
		rev.Post("/versions/{id}", m.handleDecision)
		rev.Get("/publishers", m.handlePublishers)
		rev.Post("/publishers/{id}/verify", m.handleVerifyPublisher)
	})
}

// handleQueue lists what is waiting, oldest first.
//
// A queue is answered in the order people joined it. Newest-first would mean a
// submission from a publisher nobody is chasing sinks quietly to the bottom.
func (m *Module) handleQueue(w http.ResponseWriter, r *http.Request) {
	pending, err := m.store.ListReviewQueue(r.Context())
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load the review queue")
		return
	}
	nexus.JSON(w, http.StatusOK, pending)
}

func (m *Module) handleHistory(w http.ResponseWriter, r *http.Request) {
	history, err := m.store.ReviewHistory(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load the review history")
		return
	}
	nexus.JSON(w, http.StatusOK, history)
}

// handleDecision publishes, rejects or withdraws a version.
//
// Every outcome is recorded with who decided it and why, because "why is this
// app not in the store" is a question a publisher is entitled to an answer to,
// and because a decision nobody is named for is one nobody can be asked about.
func (m *Module) handleDecision(w http.ResponseWriter, r *http.Request) {
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
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		nexus.Error(w, http.StatusBadRequest, "malformed request body")
		return
	}
	switch body.Action {
	case "publish", "reject", "yank":
	default:
		nexus.Error(w, http.StatusBadRequest, `action must be "publish", "reject" or "yank"`)
		return
	}
	// A refusal with no reason is a refusal a publisher cannot act on. Publishing
	// needs no note — the version speaks for itself — but turning one down does.
	if body.Action != "publish" && strings.TrimSpace(body.Note) == "" {
		nexus.Error(w, http.StatusBadRequest,
			"say why: a rejected or withdrawn version needs a reason its publisher can act on")
		return
	}

	versionID := chi.URLParam(r, "id")
	err = m.store.DecideVersion(r.Context(), versionID, body.Action, claims.UserID,
		claims.Email, strings.TrimSpace(body.Note))
	if errors.Is(err, registry.ErrNotFound) {
		nexus.Error(w, http.StatusNotFound, "no such version")
		return
	}
	if err != nil {
		slog.Error("could not record a review decision", "error", err, "version_id", versionID)
		nexus.Error(w, http.StatusInternalServerError, "could not record the decision")
		return
	}
	nexus.Audit(r.Context(), tenantID, claims.UserID, "review."+body.Action, "store_app_version",
		map[string]any{"version_id": versionID, "note": body.Note})
	nexus.JSON(w, http.StatusOK, map[string]string{"status": body.Action})
}

func (m *Module) handlePublishers(w http.ResponseWriter, r *http.Request) {
	publishers, err := m.store.ListPublishers(r.Context())
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load publishers")
		return
	}
	nexus.JSON(w, http.StatusOK, publishers)
}

// handleVerifyPublisher is the decision that a publisher is who they say.
//
// On this platform that claim has an answer already: a tenant whose legal
// entity has been confirmed against the national registry. This records that a
// reviewer accepted it, which is a different fact from the confirmation itself
// and the one a storefront badge should rest on.
func (m *Module) handleVerifyPublisher(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := nexus.RequireTenant(w, r)
	if !ok {
		return
	}
	claims, err := nexus.UserFromContext(r.Context())
	if err != nil {
		nexus.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	verified := r.URL.Query().Get("verified") != "false"
	publisherID := chi.URLParam(r, "id")
	if err := m.store.SetPublisherVerified(r.Context(), publisherID, verified, claims.UserID); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			nexus.Error(w, http.StatusNotFound, "no such publisher")
			return
		}
		nexus.Error(w, http.StatusInternalServerError, "could not record the decision")
		return
	}
	nexus.Audit(r.Context(), tenantID, claims.UserID, "review.publisher_verified", "publisher",
		map[string]any{"publisher_id": publisherID, "verified": verified})
	nexus.JSON(w, http.StatusOK, map[string]any{"verified": verified})
}
