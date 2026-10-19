FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY scheduler-service/go.mod scheduler-service/go.sum ./scheduler-service/
RUN cd scheduler-service && go mod download
COPY scheduler-service/ ./scheduler-service/
RUN cd scheduler-service && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/scheduler-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates fish postgresql-client openssh-client sshpass
WORKDIR /app
COPY --from=builder /out/scheduler-service .
EXPOSE 8084
CMD ["./scheduler-service"]
