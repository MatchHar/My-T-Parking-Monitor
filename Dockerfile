FROM golang:1.25.12-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS build
WORKDIR /src
COPY go.mod ./
COPY go.sum ./
RUN go mod download
# VERSION is embedded into the binary (//go:embed) — keep in sync with release tags.
COPY VERSION ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/states-api .

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
RUN apk add --no-cache ca-certificates tzdata
RUN addgroup -S -g 10001 myt && adduser -S -D -H -u 10001 -G myt myt
RUN install -d -o 10001 -g 10001 -m 0700 /data
WORKDIR /app
COPY --from=build /out/states-api /app/states-api
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/app/states-api"]
