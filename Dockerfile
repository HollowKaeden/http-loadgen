# Образ веб-сервиса для формы на сайте. Внутри оба бинаря: loadgen-web (по умолчанию)
# и CLI loadgen, чтобы на сервере можно было вручную погонять нагрузку тем же кодом.

FROM golang:1.25-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# Статическая сборка без cgo: бинарю не нужны библиотеки из образа.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/loadgen-web ./cmd/loadgen-web \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/loadgen ./cmd/loadgen

FROM alpine:3.24
# ca-certificates нужны, чтобы ходить на цели по https; wget из busybox для healthcheck.
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
COPY --from=build /out/ /usr/local/bin/
USER app
WORKDIR /app
EXPOSE 8080
ENTRYPOINT ["loadgen-web"]
CMD ["serve"]
