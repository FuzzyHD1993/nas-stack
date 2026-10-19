FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY notify-service/go.mod notify-service/go.sum ./notify-service/
RUN cd notify-service && go mod download
COPY notify-service/ ./notify-service/
RUN cd notify-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/notify-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata libnotify
WORKDIR /app
COPY --from=builder /out/notify-service .
EXPOSE 8083
CMD ["./notify-service"]
