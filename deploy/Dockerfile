FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder
ARG TARGETOS TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN curl -sf https://openrouter.ai/api/v1/models -o internal/llm/catalog/openrouter_models.json || true
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /awaki ./cmd/awaki

# -----------------------------------------------------------

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash curl wget git ca-certificates gnupg jq \
    ripgrep fd-find grep sed gawk less tree \
    zip unzip tar gzip rsync \
    netcat-openbsd dnsutils iputils-ping traceroute openssh-client \
    python3 python3-pip python3-venv nodejs npm sqlite3 \
    postgresql-client redis-tools \
    htop procps lsof file xxd bc \
    && rm -rf /var/lib/apt/lists/*

RUN pip3 install --no-cache-dir --break-system-packages \
    requests httpx beautifulsoup4 pandas || true

ARG TARGETARCH
RUN curl -fsSL "https://github.com/mikefarah/yq/releases/latest/download/yq_linux_${TARGETARCH}" \
    -o /usr/local/bin/yq && chmod +x /usr/local/bin/yq || true

RUN ln -sf /usr/bin/fdfind /usr/local/bin/fd || true

COPY --from=builder /awaki /usr/local/bin/awaki

ENV AWAKI_DATA_DIR=/data
ENV AWAKI_WORK_DIR=/work
RUN mkdir -p /data /work

WORKDIR /work

ENTRYPOINT ["awaki"]
