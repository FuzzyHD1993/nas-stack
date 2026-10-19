FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY gateway/go.mod gateway/go.sum ./gateway/
RUN cd gateway && go mod download
COPY gateway/ ./gateway/
RUN cd gateway && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/gateway .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/gateway .
EXPOSE 8080 9091
CMD ["./gateway"]
