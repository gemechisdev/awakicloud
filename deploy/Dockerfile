FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder
ARG TARGETOS TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN curl -sf https://openrouter.ai/api/v1/models -o internal/llm/catalog/openrouter_models.json || true
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /awaki ./cmd/awaki

# -----------------------------------------------------------
# Runtime: agent binary + practical dev toolchain for in-container work
# -----------------------------------------------------------

FROM ubuntu:24.04

ARG TARGETARCH
ARG GO_VERSION=1.25.0

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    # shells & core
    bash curl wget git ca-certificates gnupg jq \
    # search / text
    ripgrep fd-find grep sed gawk less tree \
    # archives
    zip unzip tar gzip rsync \
    # network diagnostics
    netcat-openbsd dnsutils iputils-ping traceroute openssh-client \
    # languages commonly used by the agent
    python3 python3-pip python3-venv nodejs npm sqlite3 \
    # DB clients
    postgresql-client redis-tools \
    # process / inspect
    htop procps lsof file xxd bc \
    # compile CGO / native deps when needed
    build-essential make pkg-config gcc g++ \
    # editors (lightweight)
    nano vim-tiny \
    && rm -rf /var/lib/apt/lists/*

# Python extras (optional; ignore failures on tiny free images)
RUN pip3 install --no-cache-dir --break-system-packages \
    requests httpx beautifulsoup4 pandas || true

# yq
RUN curl -fsSL "https://github.com/mikefarah/yq/releases/latest/download/yq_linux_${TARGETARCH}" \
    -o /usr/local/bin/yq && chmod +x /usr/local/bin/yq || true

# fd alias (Debian packages it as fdfind)
RUN ln -sf /usr/bin/fdfind /usr/local/bin/fd || true

# --- Go toolchain (matches module go 1.25) ---
# Agent can `go build` / `go test` / `go run` inside the container.
RUN set -eux; \
    arch="${TARGETARCH:-amd64}"; \
    case "$arch" in \
      amd64|arm64) ;; \
      *) arch=amd64 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o /tmp/go.tgz; \
    rm -rf /usr/local/go; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm -f /tmp/go.tgz; \
    /usr/local/go/bin/go version

ENV PATH="/usr/local/go/bin:/root/go/bin:${PATH}"
ENV GOPATH=/root/go
ENV GOTOOLCHAIN=local
# Module cache under data volume when possible
ENV GOCACHE=/data/.cache/go-build
ENV GOMODCACHE=/data/.cache/go-mod

COPY --from=builder /awaki /usr/local/bin/awaki

ENV AWAKI_DATA_DIR=/data
ENV AWAKI_WORK_DIR=/work
RUN mkdir -p /data /work /data/.cache/go-build /data/.cache/go-mod /root/go/bin

WORKDIR /work

ENTRYPOINT ["awaki"]
