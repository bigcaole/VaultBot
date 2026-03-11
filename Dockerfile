FROM golang:1.23-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o vaultbot ./cmd/server

FROM postgres:18-alpine
RUN apk add --no-cache openssl ca-certificates tzdata \
    && adduser -D -g '' vaultbot
USER vaultbot
WORKDIR /home/vaultbot
COPY --from=build /app/vaultbot /usr/local/bin/vaultbot
EXPOSE 8080
ENTRYPOINT ["vaultbot"]
