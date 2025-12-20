FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o /app/server .

FROM alpine:3.23

WORKDIR /app

RUN addgroup -S appgroup && adduser -S appuser -G appgroup

COPY --from=builder /app/server .

RUN chown appuser:appgroup /app/server

EXPOSE 8080

USER appuser

CMD [ "./server" ]