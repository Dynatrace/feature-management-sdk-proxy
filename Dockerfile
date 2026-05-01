FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /sdk-proxy ./cmd/proxy

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /sdk-proxy /sdk-proxy
EXPOSE 8080
ENTRYPOINT ["/sdk-proxy"]
