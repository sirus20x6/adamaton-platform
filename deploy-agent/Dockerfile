# Multi-stage build: golang on trixie compiles the binary, trixie-slim
# runs it. Runtime stage installs docker-ce-cli + the compose plugin so
# `docker compose pull/up` works inside the container against the host's
# socket (bind-mounted at /var/run/docker.sock).

FROM golang:1.25-trixie AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/deploy-agent ./cmd/deploy-agent

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates tzdata curl gnupg \
 && install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg \
 && chmod a+r /etc/apt/keyrings/docker.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian trixie stable" \
      > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
 && apt-get purge -y curl gnupg \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/deploy-agent /usr/local/bin/deploy-agent
EXPOSE 9128
CMD ["/usr/local/bin/deploy-agent"]
