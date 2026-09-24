IMAGE ?= forgejo-arc:dev
PROFILE ?= forgejo-arc
KUBECTL ?= kubectl --context=$(PROFILE)

.PHONY: generate lint test build image install deploy minikube e2e chart
generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0 object crd paths=./api/... output:crd:artifacts:config=config/crd
	mkdir -p charts/forgejo-arc/crds
	cp config/crd/*.yaml charts/forgejo-arc/crds/
lint:
	@set -eu; unformatted="$$(gofmt -l $$(git ls-files '*.go'))"; \
	if [ -n "$$unformatted" ]; then printf 'Run gofmt on:\n%s\n' "$$unformatted"; exit 1; fi
test:
	go test -race ./...
	go vet ./...
build:
	CGO_ENABLED=0 go build -trimpath -o bin/controller ./cmd/controller
image:
	docker build -t $(IMAGE) .
minikube:
	minikube start -p $(PROFILE) --driver=docker --kubernetes-version=v1.34.5 --cpus=4 --memory=6144
install:
	$(KUBECTL) apply -f config/crd
deploy: install
	$(KUBECTL) apply -f config/manager.yaml
e2e:
	bash test/e2e/run.sh
chart:
	helm lint charts/forgejo-arc
	helm template forgejo-arc charts/forgejo-arc >/dev/null
