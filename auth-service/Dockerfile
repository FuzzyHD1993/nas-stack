FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY auth-service/go.mod auth-service/go.sum ./auth-service/
RUN cd auth-service && go mod download
COPY auth-service/ ./auth-service/
RUN cd auth-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/auth-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/auth-service .
EXPOSE 8086
CMD ["./auth-service"]
