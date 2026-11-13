FROM --platform=$BUILDPLATFORM golang:1.24.1-alpine AS build

RUN apk add --no-cache ca-certificates

ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/placement ./internal/placement
COPY internal/authorization ./internal/authorization
COPY cmd/pve-placement-authorizer ./cmd/pve-placement-authorizer
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/pve-placement-authorizer ./cmd/pve-placement-authorizer

FROM scratch
COPY --from=build /out/pve-placement-authorizer /usr/local/bin/pve-placement-authorizer
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/pve-placement-authorizer"]
