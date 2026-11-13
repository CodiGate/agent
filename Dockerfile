# Build from this directory: docker build -t codigate-agent .
ARG GO_VERSION=1.27.4
# Cross-compiles on the build machine, so a multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
ARG VERSION=dev
ARG TARGETOS TARGETARCH TARGETVARIANT
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/codigate-agent ./cmd/codigate-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/codigate-agent /usr/local/bin/codigate-agent
ENV CODIGATE_CONFIG=/etc/codigate/codigate.yml
ENTRYPOINT ["/usr/local/bin/codigate-agent"]
