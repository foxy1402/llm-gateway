# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder
WORKDIR /src
# Cache deps first.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite is pure Go → CGO_ENABLED=0 static binary works in distroless.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags="-s -w" -trimpath -o /gateway ./cmd/gateway
# Pre-create the data dir owned by the nonroot user (uid 65532) so a fresh,
# never-before-mounted volume can still be opened by SQLite. Works because the
# empty /data created here is baked into the image layer with the right owner.
RUN mkdir -p /data && chown 65532:65532 /data

FROM gcr.io/distroless/static:nonroot
# Distroless nonroot runs as uid 65532. /data is the DB mount point.
COPY --from=builder /gateway /gateway
# Bring in the pre-chowned /data dir (ownership 65532) before switching user.
COPY --from=builder --chown=nonroot:nonroot /data /data
USER nonroot:nonroot
EXPOSE 8080
ENV GATEWAY_LISTEN=":8080" \
    DB_PATH="/data/gateway.db"
VOLUME ["/data"]
ENTRYPOINT ["/gateway"]
