.PHONY: build test test-int vet lint docker clean

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

docker:
	docker build -t sdk-proxy .

clean:
	rm -f sdk-proxy
