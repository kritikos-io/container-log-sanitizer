FROM golang:1.27.2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./... && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/container-log-sanitizer .

FROM scratch
COPY --from=build /out/container-log-sanitizer /container-log-sanitizer
ENTRYPOINT ["/container-log-sanitizer"]
