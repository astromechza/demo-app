FROM golang:1.25.5-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /demo-app

FROM alpine:3.23
COPY --from=builder /demo-app /demo-app
USER 65534
ENTRYPOINT ["/demo-app"]
