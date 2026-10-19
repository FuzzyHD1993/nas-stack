FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY host-agent/go.mod host-agent/go.sum ./host-agent/
RUN cd host-agent && go mod download
COPY host-agent/ ./host-agent/
RUN cd host-agent && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/host-agent .

FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata lm-sensors smartmontools procps
WORKDIR /app
COPY --from=builder /out/host-agent .
EXPOSE 9998
CMD ["./host-agent"]
