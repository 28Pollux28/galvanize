FROM golang:1.25 AS builder

WORKDIR /app
COPY ./galvanize-instancer .
RUN go build -o galvanize .

FROM debian:stable-slim
LABEL authors="28Pollux28"

RUN useradd -ms /bin/bash galvanize

# Install curl to allow health checks and ansible
RUN apt-get update && apt-get install -y curl pipx ssh-client \
    && rm -rf /var/lib/apt/lists/* \
    && pipx ensurepath --global \
    && pipx install --include-deps --global ansible
WORKDIR /app
RUN mkdir "data"
RUN mkdir -p /home/galvanize/.ssh \
    && chown -R galvanize:galvanize /home/galvanize/ \
    && chmod 700 /home/galvanize/.ssh
#COPY ./config.yaml ./data
COPY --from=builder /app/galvanize .
RUN chown -R galvanize:galvanize /app


USER galvanize

EXPOSE 8080

CMD ["./galvanize serve 8080 -c config.yaml"]