/*
 * Gerege Nexus App Store
 * Copyright (c) 2026 Gerege Systems Development Team, Gerege Nomadica Foundation
 * Distributed under the Apache 2.0 License.
 *
 * The whole of this product's wiring.
 *
 * There is no platform code here and there is not meant to be: sign-in,
 * tenants, the database and its isolation, the store gate, the menu and the
 * HTTP server are the core's, taken as a dependency by tag. What this
 * repository adds is three modules and the line that registers them.
 *
 * If this file ever grows business logic, it belongs in a module instead — see
 * CONTRIBUTING in the core repository, rule 3.
 */

package main

import (
	"log/slog"
	"os"

	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/nexus"
	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/platform"

	"github.com/gerege-systems/appstore-gerege-nexus/modules/publisher"
	"github.com/gerege-systems/appstore-gerege-nexus/modules/registry"
	"github.com/gerege-systems/appstore-gerege-nexus/modules/review"
)

func main() {
	err := platform.Run(platform.Options{
		// Registered in dependency order, the way the core registers its own:
		// the registry owns the tables the other two write through, and a
		// module constructed before the thing it stores into would have to
		// resolve it later, which is a second way for the same wiring to be
		// wrong.
		Modules: func(p nexus.Platform) {
			registry.New(p)
			publisher.New(p)
			review.New(p)
		},
	})
	if err != nil {
		slog.Error("the app store stopped", "error", err)
		os.Exit(1)
	}
}
