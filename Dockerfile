FROM golang:1.24.5 AS build

LABEL org.opencontainers.image.description="Dockerized Hoosat Stratum Bridge"
LABEL org.opencontainers.image.authors="onemorebsmith,hoosat"
LABEL org.opencontainers.image.source="https://github.com/Hoosat-Oy/htn-stratum-bridge"

# Install dependencies
RUN apt-get update && apt-get install -y curl git openssh-client binutils gcc musl-dev

WORKDIR /go/src/app
COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .
RUN go build -o /go/bin/htnbridge ./cmd/htnbridge

FROM ubuntu:24.04

WORKDIR /app

COPY --from=build /go/bin/htnbridge /app/htnbridge
COPY cmd/htnbridge/config.yaml /app/config.yaml

RUN chmod +x /app/htnbridge

ENTRYPOINT ["/app/htnbridge"]
CMD ["--solo", "true"]