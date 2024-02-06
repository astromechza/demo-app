FROM golang:1-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /demo-app

FROM alpine
COPY --from=builder /demo-app /demo-app
ENTRYPOINT ["/demo-app"]
