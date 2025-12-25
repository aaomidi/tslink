FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY main.go .
RUN go mod init server && CGO_ENABLED=0 go build -o /server .

FROM alpine:3.20
COPY --from=builder /server /server
CMD ["/server"]
