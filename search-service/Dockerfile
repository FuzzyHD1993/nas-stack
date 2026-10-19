FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY search-service/go.mod search-service/go.sum ./search-service/
RUN cd search-service && go mod download
COPY search-service/ ./search-service/
RUN cd search-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/search-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/search-service .
EXPOSE 8088
CMD ["./search-service"]
