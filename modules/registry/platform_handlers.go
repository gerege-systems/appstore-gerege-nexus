/*
 * Gerege Nexus — App Store registry
 * Copyright (c) 2026 Gerege Systems Development Team, @craftzbay, Gemini AI & Claude AI
 * Distributed under the Apache 2.0 License.
 */

// Operating the audience: registering a deployment, issuing it a credential,
// and deciding which private apps it may be offered.
//
// All of it is gated. The catalogue these decisions shape is public; the
// decisions are not, and neither is the list of who has been granted what —
// that list names every private arrangement this store has, which is exactly
// the thing a private app exists to keep off a public page.

package registry

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/nexus"
	"github.com/go-chi/chi/v5"
)

// actor is who is making the decision, for the record on the row.
func actor(r *http.Request) string {
	claims, err := nexus.UserFromContext(r.Context())
	if err != nil {
		return ""
	}
	return claims.UserID
}

func (m *Module) handleListPlatforms(w http.ResponseWriter, r *http.Request) {
	platforms, err := m.store.ListPlatforms(r.Context())
	if err != nil {
		slog.Error("could not list platforms", "error", err)
		nexus.Error(w, http.StatusInternalServerError, "could not list platforms")
		return
	}
	nexus.JSON(w, http.StatusOK, platforms)
}

// handleCreatePlatform registers a deployment and answers with its token.
//
// The token is in this response and in no other. It is stored as a digest, so
// there is nothing to show a second time — an operator who loses it issues
// another, which is a smaller problem than a registry able to read back working
// credentials for every deployment in the field.
func (m *Module) handleCreatePlatform(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Origin string `json:"origin"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		nexus.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		nexus.Error(w, http.StatusBadRequest, "a platform needs a name")
		return
	}

	platform, token, err := m.store.CreatePlatform(r.Context(), body.Name, body.Origin, body.Note, actor(r))
	if err != nil {
		slog.Error("could not register a platform", "error", err)
		nexus.Error(w, http.StatusInternalServerError, "could not register the platform")
		return
	}
	nexus.Audit(r.Context(), "", actor(r), "appstore.platform_registered", "platform",
		map[string]any{"platform_id": platform.ID, "name": platform.Name})

	nexus.JSON(w, http.StatusCreated, map[string]any{
		"platform": platform,
		// Named so it reads as what it is at the one moment it exists.
		"token": token,
		"note":  "This token is shown once. Set it as APP_CATALOG_TOKEN on that deployment.",
	})
}

// handleSetPlatformEnabled revokes or restores a credential.
//
// Revoking discards that platform's cached catalogues as well, because the
// cache is signed bytes on disk: leaving them would mean a revocation that
// takes effect whenever somebody next publishes, which is not a revocation.
func (m *Module) handleSetPlatformEnabled(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		nexus.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	err := m.store.SetPlatformEnabled(r.Context(), id, body.Enabled)
	if errors.Is(err, ErrNotFound) {
		nexus.Error(w, http.StatusNotFound, "platform not found")
		return
	}
	if err != nil {
		slog.Error("could not change a platform's state", "error", err, "platform_id", id)
		nexus.Error(w, http.StatusInternalServerError, "could not change the platform")
		return
	}
	if err := m.store.DiscardPrivateSnapshots(r.Context(), id); err != nil {
		slog.Warn("could not discard the platform's cached catalogues", "error", err, "platform_id", id)
	}
	nexus.Audit(r.Context(), "", actor(r), "appstore.platform_access_changed", "platform",
		map[string]any{"platform_id": id, "enabled": body.Enabled})
	nexus.JSON(w, http.StatusOK, map[string]any{"platform_id": id, "enabled": body.Enabled})
}

// handleGrant and handleRevoke decide which private apps a platform sees.
//
// Both discard that platform's cached catalogue for the same reason: the cache
// is keyed by the registry's revision counter, and a grant changes what one
// platform may see without changing what has been published. Without this a
// grant would arrive at the deployment whenever somebody next published
// something — which is to say, on a day unrelated to the decision.
func (m *Module) handleGrant(w http.ResponseWriter, r *http.Request) {
	platformID, appID := chi.URLParam(r, "id"), chi.URLParam(r, "appID")

	app, err := m.store.AppByID(r.Context(), appID)
	if errors.Is(err, ErrNotFound) {
		nexus.Error(w, http.StatusNotFound, "app not found")
		return
	}
	if err != nil {
		nexus.Error(w, http.StatusInternalServerError, "could not load the app")
		return
	}
	// A grant for a public app would be a row that means nothing today and the
	// wrong thing tomorrow: it says "this platform may see it", which is
	// already true, and would go on saying so if the app were ever made
	// private after somebody forgot this row existed.
	if app.Visibility != "private" {
		nexus.Error(w, http.StatusBadRequest,
			"only a private app is granted to a platform; this one is public and every platform already has it")
		return
	}

	if err := m.store.Grant(r.Context(), appID, platformID, actor(r)); err != nil {
		slog.Error("could not grant an app", "error", err, "app_id", appID, "platform_id", platformID)
		nexus.Error(w, http.StatusInternalServerError, "could not grant the app")
		return
	}
	if err := m.store.DiscardPrivateSnapshots(r.Context(), platformID); err != nil {
		slog.Warn("could not discard the platform's cached catalogues", "error", err, "platform_id", platformID)
	}
	nexus.Audit(r.Context(), "", actor(r), "appstore.app_granted", "platform",
		map[string]any{"platform_id": platformID, "app_id": appID})
	nexus.JSON(w, http.StatusOK, map[string]any{"platform_id": platformID, "app_id": appID, "granted": true})
}

func (m *Module) handleRevoke(w http.ResponseWriter, r *http.Request) {
	platformID, appID := chi.URLParam(r, "id"), chi.URLParam(r, "appID")
	if err := m.store.Revoke(r.Context(), appID, platformID); err != nil {
		slog.Error("could not revoke an app", "error", err, "app_id", appID, "platform_id", platformID)
		nexus.Error(w, http.StatusInternalServerError, "could not revoke the app")
		return
	}
	if err := m.store.DiscardPrivateSnapshots(r.Context(), platformID); err != nil {
		slog.Warn("could not discard the platform's cached catalogues", "error", err, "platform_id", platformID)
	}
	nexus.Audit(r.Context(), "", actor(r), "appstore.app_revoked", "platform",
		map[string]any{"platform_id": platformID, "app_id": appID})
	nexus.JSON(w, http.StatusOK, map[string]any{"platform_id": platformID, "app_id": appID, "granted": false})
}
