# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
# The module files first, on their own layer, so a source edit does not re-download the
# world. There are three dependencies and they are all cobra, so this is cheap either way.
COPY go.mod go.sum ./
RUN go mod download
# COPY path by path rather than a .dockerignore. An allowlist cannot accidentally admit a
# local data/ directory holding real tokens, and the cost is this line: a new top-level
# directory is a new entry here, which the build fails loudly about rather than shipping
# without.
COPY main.go ./
COPY internal ./internal
ARG TARGETOS TARGETARCH VERSION=dev
# CGO off because there is nothing to link against — the whole program is the standard
# library plus cobra — which is what keeps this a static binary in an image with no
# toolchain in it.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X proxio/internal/app.Version=$VERSION" -o /out/proxio .

FROM alpine:3.22
# Load-bearing here in a way it is not in a service that only answers requests: without a
# certificate store every https:// target fails verification and proxio proxies nothing but
# plaintext.
RUN apk add --no-cache ca-certificates
COPY --from=build /out/proxio /usr/local/bin/proxio
ENV PROXIO_DATA_DIR=/data

# There is deliberately no `VOLUME /data` and no mkdir, and that absence is a feature.
# Declaring the volume would make Docker create an anonymous one, /data would always exist,
# and proxio's check for a mounted volume could never fire — which leaves the tokens living
# somewhere that disappears with the container. Without it, a container started with no -v
# refuses to start and says so. See docs/deploy.md.

EXPOSE 80
# Runs the binary's own subcommand, so the image needs no HTTP client and a wedged process
# fails it rather than passing.
HEALTHCHECK --interval=30s --timeout=5s --start-period=3s CMD ["proxio", "healthcheck"]
ENTRYPOINT ["proxio"]
CMD ["serve"]
