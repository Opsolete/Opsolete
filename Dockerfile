# syntax=docker/dockerfile:1
# Two images from one build:
#   docker build --target app    -t opsolete     .   # web UI, registry, runner API
#   docker build --target runner -t opsolete-runner .  # executes deployments with terraform

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/opsolete ./cmd/opsolete

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

# Cost estimation; only used when the runner has OPS_INFRACOST_API_KEY.
ARG INFRACOST_VERSION=0.10.47
ARG INFRACOST_SHA256_amd64=df098d5565b78cc23de57e1dac9f2a02491691322cbd690dca9b197e3e02fe93
ARG INFRACOST_SHA256_arm64=1db33dddcac989f39687fa9bf00ed9d8cce4268450578848e5bf6287ad8a7b44
RUN case "$TARGETARCH" in amd64) sha="$INFRACOST_SHA256_amd64" ;; arm64) sha="$INFRACOST_SHA256_arm64" ;; *) exit 1 ;; esac \
 && curl -fsSLo /tmp/ic.tgz "https://github.com/infracost/infracost/releases/download/v${INFRACOST_VERSION}/infracost-linux-${TARGETARCH}.tar.gz" \
 && echo "$sha  /tmp/ic.tgz" | sha256sum -c - \
 && tar -xzf /tmp/ic.tgz -C /tmp \
 && install -m 0755 "/tmp/infracost-linux-${TARGETARCH}" /usr/local/bin/infracost

FROM gcr.io/distroless/static-debian12:nonroot AS app
COPY --from=build /out/opsolete /usr/local/bin/opsolete
COPY catalog /catalog
ENV ADDR=:8080 TEMPLATES_DIR=/catalog
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/opsolete"]

# One job at a time per container; scale with replicas. git: git:: module sources;
# sh: local-exec provisioners. State never stays here: it lives on the Opsolete server's state store.
FROM alpine:3.22 AS runner
RUN apk add --no-cache ca-certificates git openssh-client \
 && adduser -D -u 10001 runner \
 && mkdir -p /home/runner/.terraform.d/plugin-cache \
 && chown -R runner:runner /home/runner
COPY --from=terraform /usr/local/bin/terraform /usr/local/bin/terraform
COPY --from=terraform /usr/local/bin/infracost /usr/local/bin/infracost
COPY --from=build /out/opsolete /usr/local/bin/opsolete
USER 10001
# Provider plugins are reused across jobs (mount a volume to keep them across restarts).
ENV HOME=/home/runner TF_PLUGIN_CACHE_DIR=/home/runner/.terraform.d/plugin-cache TF_IN_AUTOMATION=1
ENTRYPOINT ["/usr/local/bin/opsolete", "runner"]
