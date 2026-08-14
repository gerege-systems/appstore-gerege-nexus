package registry_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/catalog"

	"github.com/gerege-systems/appstore-gerege-nexus/modules/registry"
	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/nexus"
)

// The catalogue contract, held across a move.
//
// testdata/golden_catalog.json has been the agreement between the registry and
// this client since the App Store was split out: a document signed by a fixed
// key over fixed input, which the registry must reproduce byte for byte and
// this client must still accept. appcatalog's own golden test asserts the
// second half.
//
// This asserts the first half, now that the registry lives here. It is the
// reason the move is safe to make: if producing the document changed by a
// single byte on the way into this package, every instance in the field would
// reject the catalogue and silently stop taking updates — the failure mode with
// no symptom, because a rejected catalogue leaves the previous one being served
// and nothing looking wrong.
//
// The same file, byte-identical, is still in the registry's old repository. It
// stays there until that service is retired: two implementations reproducing
// one document is what makes a cutover checkable rather than hopeful.
const goldenPublicKey = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="

func goldenSigner(t *testing.T) *registry.Signer {
	t.Helper()
	// Seeded 0,1,2… It signs nothing real.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	signer, err := registry.NewSigner("golden",
		base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed)))
	if err != nil {
		t.Fatal(err)
	}
	if signer.PublicKey() != goldenPublicKey {
		t.Fatalf("the golden key changed: %s", signer.PublicKey())
	}
	return signer
}

// goldenApps is the input the golden document was signed over: one module app
// and one external app, which between them exercise every field that carries
// meaning across the boundary.
func goldenApps() []catalog.CatalogApp {
	return []catalog.CatalogApp{
		{
			ID: "io.gerege.nexus.contacts", Slug: "contacts", Name: "Contacts",
			Description: "Manage business contacts.", Category: "CRM",
			Visibility: "public", Version: "1.0.0",
			Manifest: catalog.Manifest{
				ID: "io.gerege.nexus.contacts", Name: "Contacts", Version: "1.0.0", Platform: ">=1.0.0",
				Permissions: []nexus.PermissionDefinition{{Code: "contacts.read", Name: "Read contacts"}},
			},
			Translations: map[string]catalog.CatalogAppText{"mn": {Name: "Харилцагчид"}},
		},
		{
			ID: "mn.example.hrms", Slug: "hrms", Name: "Example HRMS",
			Description: "A third-party platform.", Category: "HR",
			Visibility: "public", Version: "2026.8.0",
			Manifest: catalog.Manifest{
				ID: "mn.example.hrms", Name: "Example HRMS", Version: "2026.8.0",
				Type: catalog.TypeExternal, Platform: ">=1.0.0",
				External: &catalog.ExternalSpec{
					LaunchURL: "https://hrms.example.mn/sso/gerege", SSOClientID: "app_hrms",
					Scopes: []string{"openid", "profile"}, Embed: "new_tab",
				},
				Menus: []nexus.MenuDefinition{{
					ID: "hrms_home", Label: "Example HRMS",
					ExternalURL: "https://hrms.example.mn/sso/gerege", Icon: "share-2", Order: 10,
				}},
			},
		},
	}
}

func TestThisRegistryStillProducesTheGoldenDocument(t *testing.T) {
	// The same bytes the platform's own reader is held to. Two copies of one
	// document is exactly the shape this contract has now: the registry that
	// signs it and the platform that verifies it are two repositories, and a
	// golden file only one of them could see would stop being a contract the
	// day they disagreed.
	want, err := os.ReadFile(filepath.FromSlash("testdata/golden_catalog.json"))
	if err != nil {
		t.Fatal(err)
	}

	got, _, err := registry.SignDocument(goldenSigner(t),
		time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC), goldenApps())
	if err != nil {
		t.Fatal(err)
	}

	// Byte for byte, not "equivalent JSON": the signature covers bytes, so a
	// field order or an escape that changed is a catalogue every instance in
	// the field would reject.
	if string(append(got, '\n')) != string(want) {
		t.Fatalf("the signed document changed on the way into this package.\n got: %s\nwant: %s", got, want)
	}
}
