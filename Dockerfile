FROM --platform=$BUILDPLATFORM golang:1.26.9-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY pkg ./pkg
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X sigs.k8s.io/karpenter/pkg/operator.Version=${VERSION}" -o /controller ./cmd/controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
