# syntax=docker/dockerfile:1
# Two images from one build:
#   docker build --target app    -t form2stack     .   # web UI, registry, runner API
#   docker build --target runner -t form2stack-runner .  # executes deployments with terraform

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/form2stack ./cmd/form2stack

FROM alpine:3.22 AS terraform
ARG TARGETARCH=amd64
ARG TERRAFORM_VERSION=1.17.0
ARG TERRAFORM_SHA256_amd64=c168cab7c637a8769fdb90089ddd19de701bc6ff8aca64dc2e2185fe46f59f67
ARG TERRAFORM_SHA256_arm64=f32478134ffc6d710eec474bbd316652cdddc0f4e336973a3745e8d3a3309cdf
RUN apk add --no-cache curl unzip \
 && case "$TARGETARCH" in amd64) sha="$TERRAFORM_SHA256_amd64" ;; arm64) sha="$TERRAFORM_SHA256_arm64" ;; *) exit 1 ;; esac \
 && curl -fsSLo /tmp/tf.zip "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_${TARGETARCH}.zip" \
 && echo "$sha  /tmp/tf.zip" | sha256sum -c - \
 && unzip -q /tmp/tf.zip terraform -d /usr/local/bin

FROM gcr.io/distroless/static-debian12:nonroot AS app
COPY --from=build /out/form2stack /usr/local/bin/form2stack
COPY catalog /catalog
ENV ADDR=:8080 TEMPLATES_DIR=/catalog
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/form2stack"]

# One job at a time per container; scale with replicas. git: git:: module sources;
# sh: local-exec provisioners. State never stays here: it lives on the Form2Stack server's state store.
FROM alpine:3.22 AS runner
RUN apk add --no-cache ca-certificates git openssh-client \
 && adduser -D -u 10001 runner \
 && mkdir -p /home/runner/.terraform.d/plugin-cache \
 && chown -R runner:runner /home/runner
COPY --from=terraform /usr/local/bin/terraform /usr/local/bin/terraform
COPY --from=build /out/form2stack /usr/local/bin/form2stack
USER 10001
# Provider plugins are reused across jobs (mount a volume to keep them across restarts).
ENV HOME=/home/runner TF_PLUGIN_CACHE_DIR=/home/runner/.terraform.d/plugin-cache TF_IN_AUTOMATION=1
ENTRYPOINT ["/usr/local/bin/form2stack", "runner"]
