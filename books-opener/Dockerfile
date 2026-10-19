FROM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY internal/platform ./internal/platform
COPY books-opener/go.mod books-opener/go.sum ./books-opener/
RUN cd books-opener && go mod download
COPY books-opener/ ./books-opener/
RUN cd books-opener && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/books-opener .

FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /out/books-opener .
EXPOSE 9999
CMD ["./books-opener"]
