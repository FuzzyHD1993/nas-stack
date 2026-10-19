# Contributing

1. Fork репозиторий.
2. Ветка: `git checkout -b feature/my-feature`.
3. Изменения: `gofmt -l . && go vet ./... && go build ./... && go test ./...`.
4. Коммит: Conventional Commits (english), логи/комменты — русский.
5. Pull Request.

## Стек
- Go 1.27, Fiber **v3** (не v2), PostgreSQL 17, Redis 7, `podman compose`.
- `strings.Clone()` для `c.Path()/c.Method()` в label-ах Prometheus.
- `resp.Close()` (не `resp.Body.Close()`) у Fiber v3 client.
