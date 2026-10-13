.PHONY: build test test-int vet lint lint-md helm-lint helm-test helm-install-test helm-docs docker clean

KIND_CLUSTER ?= sdk-proxy-chart
KIND_KUBECONFIG ?= $(CURDIR)/.kind/kubeconfig
CHART_TEST_IMAGE_REPO ?= sdk-proxy
CHART_TEST_IMAGE_TAG ?= chart-test

build:
	go build -o sdk-proxy ./cmd/proxy

test:
	go test ./...

test-int:
	go test -tags=integration ./...

vet:
	go vet ./...

lint:
	golangci-lint run

lint-md:
	npx markdownlint-cli2 "**/*.md"

helm-lint:
	@command -v ct >/dev/null || { echo "ct (chart-testing) not found; see CONTRIBUTING.md"; exit 1; }
	ct lint --config ct.yaml --all

helm-test:
	helm unittest charts/*

# installs the chart into kind (reused across runs) with a locally built image, then runs helm test
helm-install-test:
	@command -v ct >/dev/null || { echo "ct (chart-testing) not found; see CONTRIBUTING.md"; exit 1; }
	docker build -t $(CHART_TEST_IMAGE_REPO):$(CHART_TEST_IMAGE_TAG) .
	mkdir -p $(dir $(KIND_KUBECONFIG))
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER) --kubeconfig $(KIND_KUBECONFIG) --wait 120s
	kind export kubeconfig --name $(KIND_CLUSTER) --kubeconfig $(KIND_KUBECONFIG)
	kind load docker-image $(CHART_TEST_IMAGE_REPO):$(CHART_TEST_IMAGE_TAG) --name $(KIND_CLUSTER)
	KUBECONFIG=$(KIND_KUBECONFIG) ct install --config ct.yaml --all \
		--helm-extra-set-args "--set image.registry= --set image.repository=$(CHART_TEST_IMAGE_REPO) --set image.tag=$(CHART_TEST_IMAGE_TAG)"

helm-docs:
	helm-docs --chart-search-root charts

docker:
	docker build -t sdk-proxy .

clean:
	rm -f sdk-proxy
