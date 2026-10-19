FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY youtube-service/go.mod youtube-service/go.sum ./youtube-service/
RUN cd youtube-service && go mod download
COPY youtube-service/ ./youtube-service/
RUN cd youtube-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/youtube-service .

FROM alpine:latest
RUN apk add --no-cache \
    yt-dlp \
    ffmpeg \
    deno \
    python3 \
    py3-pip \
    ca-certificates \
    xdg-utils
WORKDIR /app
COPY --from=builder /out/youtube-service .
EXPOSE 8089
CMD ["./youtube-service"]
