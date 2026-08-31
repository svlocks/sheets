# syntax=docker/dockerfile:1.7

FROM golang:1.24-bookworm AS source
WORKDIR /src
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .

FROM source AS test
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go test -race ./...
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go vet ./...

FROM source AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/svlocks/sheets/internal/buildinfo.Version=${VERSION} -X github.com/svlocks/sheets/internal/buildinfo.Commit=${COMMIT} -X github.com/svlocks/sheets/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /out/sheets ./cmd/sheets

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/sheets /usr/local/bin/sheets
ENTRYPOINT ["/usr/local/bin/sheets"]

