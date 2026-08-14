# The App Store's own image.
#
# It carries three binaries' worth of responsibility in two: this product's
# server — the platform plus the three store modules — and the platform's own
# migration runner, built from the exact core version go.mod pins. The
# migrations themselves come from the same place, out of the module cache, so
# the schema an image applies is the schema the code in it was written against.
# Taking them from anywhere else is how a deployment ends up one release ahead
# of its own database.

FROM golang:1.26-alpine AS build

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Dependencies first, so a change to our own code does not re-download the core.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# What this build tells an app store it is.
#
# Derived from the core version go.mod pins rather than passed in, because it
# is not a choice: PlatformVersion is what every manifest's
# `"platform": ">=1.0.0"` is checked against, and the honest answer is the
# version of the platform actually compiled into this image. Left to a build
# argument it defaulted to 0.0.0 the first time a deploy forgot to pass one,
# and the instance refused every manifest it shipped with — it could not read
# its own catalogue, so it would not start.
RUN core=$(go list -m -f '{{.Version}}' github.com/gerege-systems/open-gerege-nexus/backend) \
    && version=${core#v} \
    && echo "building as platform ${version}" \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-w -s -X github.com/gerege-systems/open-gerege-nexus/backend/internal/platform.PlatformVersion=${version}" \
    -o /bin/appstore .

# The core's migration runner, from the version this product depends on. It is
# a package in that module, so `go build` resolves it like any other import.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-w -s" \
    -o /bin/migrate github.com/gerege-systems/open-gerege-nexus/backend/cmd/migrate

# And the migration files, which are data in the module rather than code. The
# path carries the version, so this reads it back rather than guessing.
RUN core=$(go list -m -f '{{.Dir}}' github.com/gerege-systems/open-gerege-nexus/backend) \
    && mkdir -p /out/db && cp -r "$core/db/migrations" /out/db/migrations \
    && chmod -R u+w /out/db

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 appstore

WORKDIR /app

COPY --from=build /bin/appstore /app/appstore
COPY --from=build /bin/migrate  /app/migrate
COPY --from=build /out/db       /app/db
# This product's catalogue: the three store modules and the two platform apps
# they sit beside. It is what makes the instance a store.
COPY catalog /app/catalog

USER appstore
EXPOSE 8080

CMD ["/app/appstore"]
