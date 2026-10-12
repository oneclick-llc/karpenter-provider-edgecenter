GO ?= go
VERSION ?= dev

.PHONY: build smoke vet manifests
build:
	$(GO) build -trimpath -ldflags '-X sigs.k8s.io/karpenter/pkg/operator.Version=$(VERSION)' -o bin/controller ./cmd/controller
smoke:
	$(GO) test -count=1 ./...
vet:
	$(GO) vet ./...
manifests:
	kubectl kustomize deploy/crds >/dev/null
	kubectl kustomize deploy/controller >/dev/null
