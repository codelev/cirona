FROM golang:1.24 AS build
WORKDIR /app
COPY . .
ARG VERSION
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X main.version=$VERSION" -trimpath -o cirona .
RUN chmod +x cirona

FROM alpine:latest
LABEL org.opencontainers.image.title="Cirona"
LABEL org.opencontainers.image.description="Self-configuring job scheduler for Docker Swarm"
WORKDIR /app
RUN apk --update --no-cache add ca-certificates openssl tzdata
COPY --from=build /app/cirona .
CMD ["./cirona"]
EXPOSE 9100
HEALTHCHECK --interval=1m --timeout=5s --start-period=5s --retries=1 CMD wget --spider --quiet http://127.0.0.1:9100 || exit 1
