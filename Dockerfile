# ================================
# pf — pmacctd(nfprobe exporter) + nfacctd(print collector) 单配置文件版
#
# 目标：
# - exporter: pmacctd 抓网卡(pcap_interface) -> nfprobe 导出 IPFIX 到 receiver
# - collector: nfacctd 监听 UDP 9995 -> print(csv) 输出到 stdout，由 processor 接收
#
# 特性：
# - processor采用批量处理优化，支持高吞吐量数据处理
# - 支持异步文件上传和状态报告
# - 配置灵活，支持多种部署模式
#
# 构建：
#   docker build -t pf:latest .
# 多平台：
#   docker buildx build --platform linux/amd64,linux/arm64 -t pf:latest .
#
# 运行（本机自收自解析；processor 读取 stdout，需挂载 pmacct.conf 与数据目录）：
#   docker run -d --name pf \
#     --network host --privileged \
#     -e PROCESSOR_CONFIG=/etc/pmacct/pmacct.conf \
#     -e PROCESSOR_DATA_DIR=/var/lib/processor \
#     -v /path/to/pmacct.conf:/etc/pmacct/pmacct.conf:ro \
#     -v "$PWD/data:/var/lib/processor" \
#     -v "$PWD/log:/var/log/pmacct" \
#     pf:latest
#
# 运行（导出到远端 192.168.70.185:9995；本容器 collector 仍监听 9995 可选）：
#   docker run -d --name pf \
#     --network host --privileged \
#     -e PROCESSOR_CONFIG=/etc/pmacct/pmacct.conf \
#     -e PROCESSOR_DATA_DIR=/var/lib/processor \
#     -v /path/to/pmacct.conf:/etc/pmacct/pmacct.conf:ro \
#     -v "$PWD/data:/var/lib/processor" \
#     -v "$PWD/log:/var/log/pmacct" \
#     pf:latest
#
#
# 配置说明：
# - pmacct 的抓包/导出/采集配置请直接写在 pmacct.conf 中
# ================================


# ================================
# 2) build: 编译 processor
# ================================
FROM golang:1.21-bookworm AS processor-builder

WORKDIR /src/processor

# 先复制依赖文件以优化构建缓存
COPY processor/go.mod processor/go.sum ./
RUN go mod download

# 再复制源代码
COPY processor/ ./
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -o /out/processor ./cmd/processor


# ================================
# 3) runtime: supervisor + tcpdump 等调试工具
# ================================
FROM debian:bookworm-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive
ENV TZ=Asia/Shanghai
ENV PROCESSOR_CONFIG=/etc/pmacct/pmacct.conf \
    PROCESSOR_DATA_DIR=/var/lib/processor \
    PROCESSOR_LOG_LEVEL=info

RUN set -eux; \
    rm -f /etc/apt/sources.list.d/debian.sources || true; \
    echo "deb http://mirrors.aliyun.com/debian bookworm main contrib non-free non-free-firmware" > /etc/apt/sources.list; \
    echo "deb http://mirrors.aliyun.com/debian bookworm-updates main contrib non-free non-free-firmware" >> /etc/apt/sources.list; \
    echo "deb http://mirrors.aliyun.com/debian-security bookworm-security main contrib non-free non-free-firmware" >> /etc/apt/sources.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      libpcap0.8 \
      bash procps tcpdump netcat-traditional iproute2 net-tools dnsutils psmisc vim less ftp \
      supervisor tzdata; \
    ln -fs /usr/share/zoneinfo/Asia/Shanghai /etc/localtime; \
    echo "Asia/Shanghai" > /etc/timezone; dpkg-reconfigure -f noninteractive tzdata; \
    rm -rf /var/lib/apt/lists/*

# pmacct binaries + libs (default: prebuilt tar; optional: build from source)
ARG PMACCT_MODE=prebuilt
ARG PMACCT_VERSION=1.7.9
ARG TARGETARCH
COPY pmacct-usrlocal-${TARGETARCH}.tar /tmp/pmacct.tar
RUN set -eux; \
    if [ "${PMACCT_MODE}" = "source" ]; then \
      apt-get update; \
      apt-get install -y --no-install-recommends \
        ca-certificates curl \
        build-essential g++ \
        libpcap-dev \
        pkg-config libtool autoconf automake make bash; \
      rm -rf /var/lib/apt/lists/*; \
      cd /tmp; \
      url="https://sourceforge.net/projects/pmacct.mirror/files/v1.7.9/pmacct-${PMACCT_VERSION}.tar.gz/download"; \
      curl -fL --retry 5 --retry-delay 2 --retry-all-errors -o pmacct.tar.gz "$url"; \
      tar xzf pmacct.tar.gz; \
      cd "pmacct-${PMACCT_VERSION}"; \
      ./configure --prefix=/usr/local; \
      make -j"$(nproc)"; \
      make install; \
      ldconfig; \
      cd /tmp; \
      rm -rf "pmacct-${PMACCT_VERSION}" pmacct.tar.gz; \
      apt-get purge -y --auto-remove \
        build-essential g++ libpcap-dev pkg-config libtool autoconf automake make; \
      rm -rf /var/lib/apt/lists/*; \
    else \
      tar -xf /tmp/pmacct.tar -C /tmp; \
      cp -a "/tmp/pmacct-usrlocal-${TARGETARCH}/." /usr/local/; \
      rm -rf "/tmp/pmacct-usrlocal-${TARGETARCH}" /tmp/pmacct.tar; \
    fi
# processor binary
COPY --from=processor-builder /out/processor /usr/local/bin/processor

# 创建目录并设置权限，添加错误处理
RUN set -eux; \
    mkdir -p /etc/pmacct /var/log/pmacct /var/log/supervisor /var/run/supervisor /opt/pf /var/lib/processor || { echo "Failed to create directories"; exit 1; } && \
    chmod 755 /var/run/supervisor || { echo "Failed to set directory permissions"; exit 1; } && \
    # 验证关键目录存在
    test -d /etc/pmacct && test -d /var/log/pmacct && test -d /var/log/supervisor && test -d /var/run/supervisor && test -d /opt/pf && test -d /var/lib/processor || { echo "Directory verification failed"; exit 1; }

# 默认配置：使用构建上下文中的 pmacct.conf
COPY pmacct.conf /etc/pmacct/pmacct.conf

# 使用项目内的 pmacct.conf 作为默认配置（若用户挂载则覆盖）
RUN cat >/usr/local/bin/render_pmacct_conf.sh <<'EOF'
#!/usr/bin/env bash
set -eu
CONF="${PMACCT_CONF:-/etc/pmacct/pmacct.conf}"

if [ -s "${CONF}" ]; then
  echo "[render] Using existing config: ${CONF}"
  exit 0
fi

echo "[render] ERROR: Config not found: ${CONF}"
exit 1
EOF

# exporter 启动脚本
RUN cat >/usr/local/bin/start_exporter.sh <<'EOF'
#!/usr/bin/env bash
set -eu
echo "[exporter] Starting pmacctd exporter..."

# 渲染配置文件
if ! /usr/local/bin/render_pmacct_conf.sh; then
    echo "[exporter] Failed to render pmacct config"
    exit 1
fi

CONF="${PMACCT_CONF:-/etc/pmacct/pmacct.conf}"
RUNTIME_DIR="/var/run/pmacct"
FILTERED_CONF="${RUNTIME_DIR}/pmacctd.conf"

# 创建运行时目录
if ! mkdir -p "${RUNTIME_DIR}"; then
    echo "[exporter] Failed to create runtime directory: ${RUNTIME_DIR}"
    exit 1
fi

# 过滤掉 processor_* 配置，避免 pmacctd 报未知项
if ! awk '
  /^[[:space:]]*processor_/ {next}
  /^[[:space:]]*#[[:space:]]*processor_/ {next}
  {print}
' "${CONF}" > "${FILTERED_CONF}"; then
    echo "[exporter] Failed to filter config file"
    exit 1
fi

echo "[exporter] Using config: ${FILTERED_CONF}"
echo "[exporter] Starting pmacctd..."
exec /usr/local/sbin/pmacctd -f "${FILTERED_CONF}"
EOF

# collector 启动脚本（输出到 stdout，由 processor 接收）
RUN cat >/usr/local/bin/start_collector.sh <<'EOF'
#!/usr/bin/env bash
set -eu
echo "[collector] Starting nfacctd collector with processor..."

# 渲染配置文件
if ! /usr/local/bin/render_pmacct_conf.sh; then
    echo "[collector] Failed to render pmacct config"
    exit 1
fi

CONF="${PMACCT_CONF:-/etc/pmacct/pmacct.conf}"
RUNTIME_DIR="/var/run/pmacct"
FILTERED_CONF="${RUNTIME_DIR}/nfacctd.conf"
PROCESSOR_BIN="${PROCESSOR_BIN:-/usr/local/bin/processor}"
PROCESSOR_CONFIG="${PROCESSOR_CONFIG:-/etc/pmacct/pmacct.conf}"
PROCESSOR_DATA_DIR="${PROCESSOR_DATA_DIR:-/var/lib/processor}"
PROCESSOR_LOG_LEVEL="${PROCESSOR_LOG_LEVEL:-info}"

# 检查processor二进制文件
if [ ! -x "${PROCESSOR_BIN}" ]; then
  echo "[collector] ERROR: processor not found or not executable: ${PROCESSOR_BIN}"
  exit 1
fi

# 创建必要的目录
if ! mkdir -p "${RUNTIME_DIR}" "${PROCESSOR_DATA_DIR}"; then
    echo "[collector] ERROR: Failed to create directories"
    exit 1
fi

echo "[collector] Using config: ${CONF}"
echo "[collector] Processor binary: ${PROCESSOR_BIN}"

# nfacctd 不支持 nfprobe 插件；过滤 processor_* 与 nfprobe_*，并强制 plugins: print
if ! awk '
  BEGIN {plugins_set=0}
  /^[[:space:]]*plugins:/ {print "plugins: print"; plugins_set=1; next}
  /^[[:space:]]*nfprobe_/ {next}
  /^[[:space:]]*processor_/ {next}
  /^[[:space:]]*#[[:space:]]*processor_/ {next}
  {print}
  END {if (!plugins_set) print "plugins: print"}
' "${CONF}" > "${FILTERED_CONF}"; then
    echo "[collector] ERROR: Failed to filter config file"
    exit 1
fi

echo "[collector] Filtered config: ${FILTERED_CONF}"
echo "[collector] Processor config: ${PROCESSOR_CONFIG}"
echo "[collector] Processor data dir: ${PROCESSOR_DATA_DIR}"
echo "[collector] Processor log level: ${PROCESSOR_LOG_LEVEL}"

# 启动nfacctd并将其输出管道到processor
echo "[collector] Starting nfacctd and processor pipeline..."
set -o pipefail
if ! /usr/local/sbin/nfacctd -f "${FILTERED_CONF}" | \
  "${PROCESSOR_BIN}" -config "${PROCESSOR_CONFIG}" -data-dir "${PROCESSOR_DATA_DIR}" -log-level "${PROCESSOR_LOG_LEVEL}"; then
    echo "[collector] ERROR: Pipeline failed"
    exit 1
fi
EOF

# 设置脚本权限
RUN set -eux; \
    sed -i 's/\r$//' /usr/local/bin/render_pmacct_conf.sh /usr/local/bin/start_exporter.sh /usr/local/bin/start_collector.sh && \
    chmod +x /usr/local/bin/render_pmacct_conf.sh /usr/local/bin/start_exporter.sh /usr/local/bin/start_collector.sh

# ================================
# supervisor：默认同时起 exporter + collector
# ================================
RUN cat >/usr/local/bin/supervisor_entry.sh <<'EOF'
#!/usr/bin/env bash
set -eu

# 生成 supervisord 配置
cat >/etc/supervisor/supervisord.conf <<'CONF'
[unix_http_server]
file=/var/run/supervisor/supervisor.sock
chmod=0700

[supervisord]
nodaemon=true
logfile=/var/log/supervisor/supervisord.log
logfile_maxbytes=50MB
logfile_backups=10
loglevel=info
pidfile=/var/run/supervisor/supervisord.pid
user=root
minfds=1024
minprocs=200

[rpcinterface:supervisor]
supervisor.rpcinterface_factory = supervisor.rpcinterface:make_main_rpcinterface

[supervisorctl]
serverurl=unix:///var/run/supervisor/supervisor.sock
CONF

cat >>/etc/supervisor/supervisord.conf <<'CONF'

[program:collector]
command=/bin/bash /usr/local/bin/start_collector.sh
autorestart=true
startsecs=2
startretries=10
priority=100
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stdout_logfile_backups=0
stderr_logfile=/dev/stdout
stderr_logfile_maxbytes=0
stderr_logfile_backups=0
redirect_stderr=false
stopsignal=TERM
stopwaitsecs=30
CONF

cat >>/etc/supervisor/supervisord.conf <<'CONF'

[program:exporter]
command=/bin/bash /usr/local/bin/start_exporter.sh
autorestart=true
startsecs=2
startretries=10
priority=200
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stdout_logfile_backups=0
stderr_logfile=/dev/stdout
stderr_logfile_maxbytes=0
stderr_logfile_backups=0
redirect_stderr=false
stopsignal=TERM
stopwaitsecs=30
CONF

exec /usr/bin/supervisord -c /etc/supervisor/supervisord.conf
EOF

# 设置supervisor_entry.sh脚本权限
RUN set -eux; \
    sed -i 's/\r$//' /usr/local/bin/supervisor_entry.sh && \
    chmod +x /usr/local/bin/supervisor_entry.sh

WORKDIR /opt/pf
ENTRYPOINT ["/usr/local/bin/supervisor_entry.sh"]
CMD []
