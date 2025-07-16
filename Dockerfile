FROM golang:1.24.5 as builder

LABEL org.opencontainers.image.description="Dockerized Hoosat Stratum Bridge"
LABEL org.opencontainers.image.authors="onemorebsmith,hoosat"
LABEL org.opencontainers.image.source="https://github.com/Hoosat-Oy/htn-stratum-bridge"

# Install dependencies
RUN apt-get update && apt-get install -y curl git openssh-client binutils gcc musl-dev

WORKDIR /go/src/app
ADD go.mod .
ADD go.sum .
RUN go mod download

ADD . .
RUN go build -o /go/bin/app ./cmd/htnbridge

FROM gcr.io/distroless/base:nonroot
COPY --from=builder /go/bin/app /
COPY cmd/htnbridge/config.yaml /

WORKDIR /
ENTRYPOINT ["/app"]