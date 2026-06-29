FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod main.go ./
RUN go build -o proxy .

FROM alpine:3.20
RUN apk add --no-cache nodejs npm && npm install -g command-code
COPY --from=builder /app/proxy /usr/local/bin/proxy
EXPOSE 8080
ENTRYPOINT ["proxy"]
