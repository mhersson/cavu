# The sandbox image: every agent container and sidecar runs from it. It sets no
# ENV, so the only variables in a container are the ones CAVU passes.
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
# ^ multi-arch index digest; refresh with `docker buildx imagetools inspect debian:trixie-slim`.

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates git curl bash coreutils procps tar xz-utils unzip build-essential \
    && rm -rf /var/lib/apt/lists/*

# The instance's CA goes in before any download, so a network that inspects TLS
# with its own CA does not stop the downloads below and every tool in the image
# trusts it. The checksums below still decide what is accepted. Each
# certificate gets a file of its own, which is what update-ca-certificates reads.
COPY ca-bundle.pem /tmp/ca-bundle.pem
RUN set -eu; \
    if [ -s /tmp/ca-bundle.pem ]; then \
      awk '/-----BEGIN CERTIFICATE-----/ { n++; on = 1; f = sprintf("/usr/local/share/ca-certificates/cavu-%d.crt", n) } \
           on { print > f } \
           /-----END CERTIFICATE-----/ { on = 0; close(f) }' /tmp/ca-bundle.pem; \
      update-ca-certificates; \
    fi; \
    rm -f /tmp/ca-bundle.pem

ARG AGENT_VERSION
ARG AGENT_SHA512_AMD64
ARG AGENT_SHA512_ARM64
RUN set -eux; \
    : "${AGENT_VERSION:?}" "${AGENT_SHA512_AMD64:?}" "${AGENT_SHA512_ARM64:?}"; \
    case "$(uname -m)" in \
      x86_64) PLATFORM=x64; SHA512="${AGENT_SHA512_AMD64}" ;; \
      aarch64) PLATFORM=arm64; SHA512="${AGENT_SHA512_ARM64}" ;; \
      *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-${PLATFORM}/-/claude-code-linux-${PLATFORM}-${AGENT_VERSION}.tgz" -o /tmp/claude.tgz; \
    echo "${SHA512}  /tmp/claude.tgz" | sha512sum -c -; \
    tar -xzf /tmp/claude.tgz -C /tmp package/claude; \
    install -m 0755 /tmp/package/claude /usr/local/bin/claude; \
    rm -rf /tmp/claude.tgz /tmp/package; \
    claude --version | grep -F "${AGENT_VERSION}"

# Hand-bumped; take each sha256 from the release's SHASUMS256.txt.
ARG MISE_VERSION=v2026.11.8
ARG MISE_SHA256_AMD64=32b6abe844efa43d37d7ac4ed21d2ec41e9479d66feb19b0db8d2c548cd6e36d
ARG MISE_SHA256_ARM64=0de1450dcc3f838e372e05e8c5231423760ad3f22d14275ef340bc0a250303ce
RUN set -eux; \
    case "$(uname -m)" in \
      x86_64) PLATFORM=x64; SHA256="${MISE_SHA256_AMD64}" ;; \
      aarch64) PLATFORM=arm64; SHA256="${MISE_SHA256_ARM64}" ;; \
      *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/jdx/mise/releases/download/${MISE_VERSION}/mise-${MISE_VERSION}-linux-${PLATFORM}" -o /tmp/mise; \
    echo "${SHA256}  /tmp/mise" | sha256sum -c -; \
    install -m 0755 /tmp/mise /usr/local/bin/mise; \
    rm -f /tmp/mise; \
    mise --version

# A fresh named volume takes its ownership from the image's directory, and is
# root-owned without one.
RUN useradd --uid 1000 --user-group --home-dir /home/agent --create-home --shell /bin/bash agent \
    && mkdir -p /workspace /handoff /cache /mirror \
    && chown 1000:1000 /workspace /handoff /cache /mirror /home/agent

COPY cavu /usr/local/bin/cavu

ARG CAVU_VERSION
ARG CA_BUNDLE_SHA256
LABEL cavu.version="${CAVU_VERSION}" cavu.claude_code="${AGENT_VERSION}" cavu.ca_bundle="${CA_BUNDLE_SHA256}"

USER agent
WORKDIR /workspace
RUN cavu version && claude --version
