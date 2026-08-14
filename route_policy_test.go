package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gerege-systems/appstore-gerege-nexus/modules/publisher"
	"github.com/gerege-systems/appstore-gerege-nexus/modules/registry"
	"github.com/gerege-systems/appstore-gerege-nexus/modules/review"
	"github.com/gerege-systems/open-gerege-nexus/backend/pkg/nexus"
)

// Which of this product's routes a stranger may reach.
//
// The platform has this guard for its own routes and it did not come across
// with the split — so the three modules that moved here brought public
// endpoints and nothing checking them. That is the wrong half to lose: the
// store is the one product in the ecosystem that is *supposed* to answer
// unauthenticated callers, which is exactly why the line between the routes
// that do and the routes that must not has to be written down somewhere a
// build can read.
//
// Every module is handed the root router, so mounting a path outside the gate
// is one line and looks exactly like mounting one inside it. This list is what
// makes the difference visible: a route reachable without a session must be
// named here, and adding a name is a deliberate act in a review rather than a
// side effect of where a line was put.
var publicRoutes = []string{
	// The signed catalogue and what it is read with. An instance pulling a
	// catalogue holds no session on this host and never will — it is a server
	// somewhere else, and the signature is the trust, not the caller.
	"/api/v1/registry/*",
	// The keys that verify that signature. Public by the same reasoning as
	// /.well-known/jwks.json: a client reads it before it trusts anything.
	"/.well-known/appstore-keys.json",
}

func isPublic(pattern string) bool {
	for _, allowed := range publicRoutes {
		if allowed == pattern {
			return true
		}
		if prefix, wildcard := strings.CutSuffix(allowed, "/*"); wildcard {
			if pattern == prefix || strings.HasPrefix(pattern, prefix+"/") {
				return true
			}
		}
	}
	return false
}

// TestEveryRouteIsGatedUnlessItIsOnThePublicList mounts the product the way
// main.go does and asks each route whether it went through the gate.
//
// The gate is a spy rather than the real one: what is under test is whether a
// route was mounted behind it at all, and a real gate would need a database, a
// tenant and a session to answer that.
func TestEveryRouteIsGatedUnlessItIsOnThePublicList(t *testing.T) {
	// One flag, reset before each request. Recording by route pattern looked
	// natural and was wrong: inside middleware chi has matched the mount point
	// but not yet the leaf, so RoutePattern() returns the group rather than the
	// route and every parameterised path under a gated group reads as ungated.
	var passedGate bool
	spy := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			passedGate = true
			next.ServeHTTP(w, r)
		})
	}

	router := chi.NewRouter()
	p := nexus.NewPlatform(nil, nil)
	registry.New(p).RegisterRoutes(router, spy)
	publisher.New(p).RegisterRoutes(router, spy)
	review.New(p).RegisterRoutes(router, spy)

	var patterns []string
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		patterns = append(patterns, method+" "+route)
		return nil
	}); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	if len(patterns) == 0 {
		t.Fatal("no routes were mounted; the modules registered nothing")
	}

	for _, entry := range patterns {
		method, route, _ := strings.Cut(entry, " ")
		pattern := strings.TrimSuffix(route, "/*")
		if isPublic(pattern) {
			continue
		}
		// Reaching the handler would need a database. Whether the gate ran is
		// decided before that, so the request is made and the panic the
		// handler raises on a nil pool is caught and ignored.
		passedGate = false
		func() {
			defer func() { _ = recover() }()
			req, err := http.NewRequest(method, concreteFor(route), nil)
			if err != nil {
				t.Fatalf("%s: %v", entry, err)
			}
			router.ServeHTTP(discard{}, req)
		}()
		if !passedGate {
			t.Errorf("%s is reachable without the gate and is not on the public list", entry)
		}
	}
}

// concreteFor turns a chi pattern into a path that will route.
func concreteFor(pattern string) string {
	out := []string{}
	for _, segment := range strings.Split(pattern, "/") {
		switch {
		case segment == "*":
			out = append(out, "x")
		case strings.HasPrefix(segment, "{"):
			out = append(out, "probe")
		default:
			out = append(out, segment)
		}
	}
	return strings.Join(out, "/")
}

type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}
