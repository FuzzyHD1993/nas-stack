FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY music-service/go.mod music-service/go.sum ./music-service/
RUN cd music-service && go mod download
COPY music-service/ ./music-service/
RUN cd music-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/music-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata ffmpeg flac
WORKDIR /app
COPY --from=builder /out/music-service .
EXPOSE 8092
CMD ["./music-service"]
