.PHONY: build test test-int vet lint lint-md helm-lint helm-test helm-docs docker clean

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
	ct lint --config ct.yaml --all

helm-test:
	helm unittest charts/*

helm-docs:
	helm-docs --chart-search-root charts

docker:
	docker build -t sdk-proxy .

clean:
	rm -f sdk-proxy
