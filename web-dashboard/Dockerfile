FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY web-dashboard/go.mod web-dashboard/go.sum ./web-dashboard/
RUN cd web-dashboard && go mod download
COPY web-dashboard/ ./web-dashboard/
RUN cd web-dashboard && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/web-dashboard .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/web-dashboard .
EXPOSE 8090
CMD ["./web-dashboard"]
