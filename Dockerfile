FROM golang:1.21-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o vaultbot ./cmd/server

FROM alpine:3.20
RUN adduser -D -g '' vaultbot
USER vaultbot
WORKDIR /home/vaultbot
COPY --from=build /app/vaultbot /usr/local/bin/vaultbot
EXPOSE 8080
ENTRYPOINT ["vaultbot"]
