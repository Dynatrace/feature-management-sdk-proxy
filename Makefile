.PHONY: build test test-int vet lint lint-md helm-lint docker clean

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
	helm lint charts/sdk-proxy
	helm template sdk-proxy charts/sdk-proxy > /dev/null

docker:
	docker build -t sdk-proxy .

clean:
	rm -f sdk-proxy
