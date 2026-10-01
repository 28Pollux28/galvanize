FROM golang:1.25 AS builder

WORKDIR /app

# Copy go mod files first for better caching
COPY ./galvanize-instancer/go.mod ./galvanize-instancer/go.sum ./
RUN go mod download

# Copy source code
COPY ./galvanize-instancer .
RUN go build -o galvanize .

FROM debian:stable-slim
LABEL authors="28Pollux28"

RUN useradd -ms /bin/bash galvanize

# Install curl to allow health checks, ansible, and tini for zombie process reaping
RUN apt-get update && apt-get install -y curl pipx ssh-client tini \
    && rm -rf /var/lib/apt/lists/* \
    && pipx ensurepath --global \
    && pipx install --include-deps --global ansible
WORKDIR /app
RUN mkdir "data"
COPY ./data/playbooks ./data/playbooks
RUN mkdir -p /home/galvanize/.ssh \
    && chown -R galvanize:galvanize /home/galvanize/ \
    && chmod 700 /home/galvanize/.ssh
#COPY ./config.yaml ./data
COPY --from=builder /app/galvanize .
RUN chown -R galvanize:galvanize /app


USER galvanize

EXPOSE 8080

# Use tini as entrypoint to properly reap zombie SSH processes from Ansible
ENTRYPOINT ["/usr/bin/tini", "--"]
# The config is expected at data/config.yaml, as mounted by docker-compose.yml
CMD ["./galvanize", "serve", "--port", "8080", "--config", "data/config.yaml"]