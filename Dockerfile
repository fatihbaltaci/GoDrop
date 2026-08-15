# Build a static binary, then ship it on nothing at all.
#
# The result is a ~10MB image with no shell, no package manager and no libc,
# there is nothing in it to exploit but GoDrop itself. The health check uses the
# binary's own `health` subcommand, since there is no curl to call.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first: this layer is cached until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
# A PostHog project key is public by design (it is embedded in client-side
# JavaScript on every site that uses PostHog), so passing it as a build argument
# leaks nothing. It is empty by default so that images built from source send no
# telemetry at all. (BuildKit's SecretsUsedInArgOrEnv check flags any argument
# named "*KEY"; that warning is expected here.)
ARG POSTHOG_KEY=""
ARG POSTHOG_HOST="https://eu.i.posthog.com"
ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${COMMIT} \
      -X main.date=${DATE} \
      -X main.posthogKey=${POSTHOG_KEY} \
      -X main.posthogHost=${POSTHOG_HOST}" \
    -o /godrop ./cmd/godrop

# The data directory is created here so it carries the right ownership even when
# no volume is mounted over it.
RUN mkdir -p /data && chown 65532:65532 /data

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /godrop /godrop
COPY --from=build --chown=nonroot:nonroot /data /data

# The listen address is deliberately not set here: GoDrop picks 8747 on its
# own, and 443 when TLS is on, which a fixed value here would override.
ENV GODROP_DATA_DIR=/data

USER nonroot:nonroot
EXPOSE 8747 443 80
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/godrop", "health"]

ENTRYPOINT ["/godrop"]
