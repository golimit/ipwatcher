# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
ARG VERSION=0.1.0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/ipwatcher ./cmd/ipwatcher

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/ipwatcher /usr/local/bin/ipwatcher
COPY configs/config.example.yaml /app/config.yaml
ENV IPWATCHER_DB_PATH=/app/data/ipwatcher.db
VOLUME ["/app/data"]
ENTRYPOINT ["ipwatcher"]
CMD ["run", "-config", "/app/config.yaml"]
