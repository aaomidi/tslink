FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY main.go .
RUN go mod init client && CGO_ENABLED=0 go build -o /client .

FROM alpine:3.20
COPY --from=builder /client /client
CMD ["/client"]
