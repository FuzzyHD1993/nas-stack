FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY media-service/go.mod media-service/go.sum ./media-service/
RUN cd media-service && go mod download
COPY media-service/ ./media-service/
RUN cd media-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/media-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata ffmpeg
WORKDIR /app
COPY --from=builder /out/media-service .
EXPOSE 8091
CMD ["./media-service"]
