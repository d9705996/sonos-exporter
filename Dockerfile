FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/sonos-exporter .

FROM scratch
COPY --from=builder /out/sonos-exporter /sonos-exporter
EXPOSE 9798
USER 65532:65532
ENTRYPOINT ["/sonos-exporter"]
