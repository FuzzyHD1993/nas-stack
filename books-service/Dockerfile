FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY books-service/go.mod books-service/go.sum ./books-service/
RUN cd books-service && go mod download
COPY books-service/ ./books-service/
RUN cd books-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/books-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/books-service .
EXPOSE 8087
CMD ["./books-service"]
