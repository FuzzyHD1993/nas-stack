FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY torrent-service/go.mod torrent-service/go.sum ./torrent-service/
RUN cd torrent-service && go mod download
COPY torrent-service/ ./torrent-service/
RUN cd torrent-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/torrent-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/torrent-service .
EXPOSE 8081
CMD ["./torrent-service"]
