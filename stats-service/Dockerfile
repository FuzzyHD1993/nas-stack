FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY stats-service/go.mod stats-service/go.sum ./stats-service/
RUN cd stats-service && go mod download
COPY stats-service/ ./stats-service/
RUN cd stats-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/stats-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates procps coreutils
WORKDIR /app
COPY --from=builder /out/stats-service .
EXPOSE 8082
CMD ["./stats-service"]
