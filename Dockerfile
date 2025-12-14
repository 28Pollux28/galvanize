FROM golang:1.25 AS builder
LABEL authors="28Pollux28"

WORKDIR /app
COPY . .
RUN go build -o instancer .

FROM debian:stable-slim
LABEL authors="28Pollux28"
WORKDIR /app
COPY --from=builder /app/instancer .
COPY --from=builder /app/config.yaml .
EXPOSE 8080

CMD ["./main serve 8080 -c config.yaml"]