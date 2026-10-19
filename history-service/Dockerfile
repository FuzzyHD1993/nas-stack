FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY history-service/go.mod history-service/go.sum ./history-service/
RUN cd history-service && go mod download
COPY history-service/ ./history-service/
RUN cd history-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/history-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /out/history-service .
EXPOSE 8085
CMD ["./history-service"]
