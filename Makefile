IMAGE ?= forgejo-arc:dev
PROFILE ?= forgejo-arc
KUBECTL ?= kubectl --context=$(PROFILE)

.PHONY: generate test build image install deploy minikube
generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0 object crd paths=./api/... output:crd:artifacts:config=config/crd
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
