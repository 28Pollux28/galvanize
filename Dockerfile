FROM golang:1.25 AS builder
LABEL authors="28Pollux28"

WORKDIR /app
COPY . .
RUN go build -o instancer .

FROM debian:stable-slim
LABEL authors="28Pollux28"
WORKDIR /app
RUN mkdir "data"
COPY --from=builder /app/instancer .
COPY --from=builder /app/config.yaml ./data
EXPOSE 8080

CMD ["./instancer serve 8080 -c config.yaml"]