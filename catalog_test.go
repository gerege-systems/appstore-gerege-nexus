package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/catalog"
	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/nexus"

	"github.com/gerege-systems/appstore-gerege-nexus/modules/publisher"
	"github.com/gerege-systems/appstore-gerege-nexus/modules/registry"
	"github.com/gerege-systems/appstore-gerege-nexus/modules/review"
)

// The bundled catalogue has to agree with the binary it ships inside.
//
// The platform refuses to start on a catalogue whose apps disagree with the
// modules compiled into it — catalogue integrity is a boot failure, not a
// warning — so a disagreement here is not a stale file, it is an instance that
// does not come up. And it is easy to arrive at without touching this file:
// bumping the core is a one-line change, and a core release that renamed one of
// the platform apps this catalogue copies leaves the copy behind.
//
// That is not hypothetical. Bumping to backend/v1.5.0 renamed
// io.gerege.nexus.organisation from "Organisation & People" 1.0.0 to
// "Directory" 2.0.0, which this catalogue still described as 1.0.0 — an image
// that would have built, deployed, and then refused to boot.
//
// What this test can check is the half that lives in this repository: the three
// store modules, which it compiles and can therefore ask. The two platform apps
// it copies are checked at boot by the core, because their code is not
// importable from here — they live in the core's internal/. See README.
func TestTheBundledCatalogueAgreesWithThisBinary(t *testing.T) {
	apps, err := loadBundledCatalogue(t)
	if err != nil {
		t.Fatalf("the bundled catalogue does not load: %v", err)
	}
	if len(apps) == 0 {
		t.Fatal("the bundled catalogue is empty")
	}

	// Registering the store's own modules is what makes them askable. A nil
	// platform is enough: nothing below calls a method that touches one.
	p := nexus.NewPlatform(nil, nil)
	registry.New(p)
	publisher.New(p)
	review.New(p)

	for _, app := range apps {
		module, compiled := nexus.Get(app.ID)
		if !compiled {
			// Either a platform app from the core, or an external app. Both are
			// legitimate entries this test cannot speak for.
			continue
		}
		if module.Version() != app.Version {
			t.Errorf("%s is compiled at %s and the catalogue says %s",
				app.ID, module.Version(), app.Version)
		}
		if module.Version() != app.Manifest.Version {
			t.Errorf("%s is compiled at %s and its manifest says %s",
				app.ID, module.Version(), app.Manifest.Version)
		}
	}
}

// loadBundledCatalogue reads catalog/apps.json and the manifests beside it.
//
// Assembled here rather than borrowed, because the core's loader lives in
// `internal/` and a distribution cannot import it — which is the rule that
// makes distributions possible at all, working exactly as intended and slightly
// against us here. `pkg/catalog` gaining a loader would be the fix; until then
// this is twenty lines and every one of them is the same shape as the real one.
//
// The platform version is left empty on purpose. It is stamped into the binary
// at build time by -ldflags and lives in that same internal package, so a test
// cannot read the real one; empty skips the compatibility constraint and leaves
// everything this file is about — versions, manifests, visibility — checked.
func loadBundledCatalogue(t *testing.T) ([]catalog.CatalogApp, error) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("catalog", "apps.json"))
	if err != nil {
		return nil, err
	}
	var apps []catalog.CatalogApp
	if err := json.Unmarshal(raw, &apps); err != nil {
		return nil, err
	}
	for i := range apps {
		manifestRaw, err := os.ReadFile(filepath.Join("catalog", "manifests", apps[i].Slug+".json"))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(manifestRaw, &apps[i].Manifest); err != nil {
			return nil, err
		}
		if err := catalog.ValidateManifest(apps[i].Manifest, ""); err != nil {
			return nil, err
		}
	}
	return apps, nil
}

// Every app this store publishes about itself declares a visibility, and it has
// to be one of the two the contract names.
//
// The registry refuses an unknown value on submission; this is the same rule
// applied to the catalogue this product ships with, which does not go through
// that path.
func TestEveryBundledAppDeclaresAKnownVisibility(t *testing.T) {
	apps, err := loadBundledCatalogue(t)
	if err != nil {
		t.Fatalf("the bundled catalogue does not load: %v", err)
	}
	for _, app := range apps {
		switch app.Visibility {
		case "", catalog.VisibilityPublic, catalog.VisibilityPrivate:
		default:
			t.Errorf("%s declares visibility %q; expected %q or %q",
				app.ID, app.Visibility, catalog.VisibilityPublic, catalog.VisibilityPrivate)
		}
	}
}
