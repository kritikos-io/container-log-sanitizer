FROM golang:1.27.2 AS build
WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends jq && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./... && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/container-log-sanitizer .

FROM scratch
COPY --from=build /out/container-log-sanitizer /container-log-sanitizer
ENTRYPOINT ["/container-log-sanitizer"]
