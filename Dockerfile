# Armada server image.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/armada-server ./cmd/armada-server && \
    CGO_ENABLED=0 go build -trimpath -o /out/armadactl ./cmd/armadactl

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 armada && \
    mkdir /data && chown armada /data
COPY --from=build /out/ /usr/local/bin/
USER armada
VOLUME /data
EXPOSE 8080
ENV ARMADA_DATA_DIR=/data ARMADA_LISTEN=:8080
ENTRYPOINT ["armada-server"]
