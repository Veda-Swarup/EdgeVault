FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/edge ./cmd/edge \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/gc ./cmd/gc

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/edge /app/edge
COPY --from=build /out/gc /app/gc
COPY --from=build /src/db/migrations /app/db/migrations
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/edge"]
