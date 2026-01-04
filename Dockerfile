# ================================
# pf — pmacctd(nfprobe exporter) + nfacctd(print collector) 单配置文件版
#
# 目标：
# - exporter: pmacctd 抓网卡(pcap_interface) -> nfprobe 导出 IPFIX 到 receiver
# - collector: nfacctd 监听 UDP 9995 -> print(csv) 输出到 stdout，由 processor 接收
#
# 构建：
#   docker build -t pf:latest .
# 多平台：
#   docker buildx build --platform linux/amd64,linux/arm64 -t pf:latest .
#
# 运行（抓 eth4，本机自收自解析；processor 读取 stdout，需挂载 pmacct.conf 与数据目录）：
#   docker run -d --name pf \
#     --network host --privileged \
#     -e PCAP_IFACE=eth4 \
#     -e PROCESSOR_CONFIG=/etc/pmacct/pmacct.conf \
#     -e PROCESSOR_DATA_DIR=/var/lib/processor \
#     -v /path/to/pmacct.conf:/etc/pmacct/pmacct.conf:ro \
#     -v "$PWD/data:/var/lib/processor" \
#     -v "$PWD/log:/var/log/pmacct" \
#     pf:latest
#
# 运行（抓 eth4，导出到远端 192.168.70.185:9995；本容器 collector 仍监听 9995 可选）：
#   docker run -d --name pf \
#     --network host --privileged \
#     -e PCAP_IFACE=eth4 \
#     -e NFPROBE_RECEIVER=192.168.70.185:9995 \
#     -e PROCESSOR_CONFIG=/etc/pmacct/pmacct.conf \
#     -e PROCESSOR_DATA_DIR=/var/lib/processor \
#     -v /path/to/pmacct.conf:/etc/pmacct/pmacct.conf:ro \
#     -v "$PWD/data:/var/lib/processor" \
#     -v "$PWD/log:/var/log/pmacct" \
#     pf:latest
#
# 如果你不想在同一个容器里起 collector，可设置：
#   -e ENABLE_COLLECTOR=false
# ================================


# ================================
# 1) build: 编译 pmacct（含 pmacctd / nfacctd / nfprobe / print）
# ================================
FROM debian:bookworm-slim AS pmacct-builder

ENV DEBIAN_FRONTEND=noninteractive
ARG PMACCT_VERSION=1.7.9

RUN set -eux; \
    rm -f /etc/apt/sources.list.d/debian.sources || true; \
    echo "deb http://mirrors.aliyun.com/debian bookworm main contrib non-free non-free-firmware" > /etc/apt/sources.list; \
    echo "deb http://mirrors.aliyun.com/debian bookworm-updates main contrib non-free non-free-firmware" >> /etc/apt/sources.list; \
    echo "deb http://mirrors.aliyun.com/debian-security bookworm-security main contrib non-free non-free-firmware" >> /etc/apt/sources.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates curl \
      build-essential g++ \
      libpcap-dev \
      pkg-config libtool autoconf automake make bash; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /tmp

RUN set -eux; \
    url_https="https://www.pmacct.net/pmacct-${PMACCT_VERSION}.tar.gz"; \
    url_http="http://www.pmacct.net/pmacct-${PMACCT_VERSION}.tar.gz"; \
    ok=0; \
    for url in "$url_https" "$url_http"; do \
      for i in 1 2 3 4 5; do \
        curl -fSL --retry 3 --retry-delay 2 --retry-connrefused --retry-all-errors \
          -C - "$url" -o pmacct.tar.gz || true; \
        if tar tzf pmacct.tar.gz >/dev/null 2>&1; then \
          ok=1; \
          break; \
        fi; \
        rm -f pmacct.tar.gz; \
        sleep 2; \
      done; \
      if [ "$ok" = "1" ]; then break; fi; \
    done; \
    test "$ok" = "1"; \
    tar xzf pmacct.tar.gz; \
    cd "pmacct-${PMACCT_VERSION}"; \
    ./configure --prefix=/usr/local; \
    make -j"$(nproc)"; \
    make install; \
    ldconfig; \
    cd /tmp; \
    rm -rf "pmacct-${PMACCT_VERSION}" pmacct.tar.gz


# ================================
# 2) build: 编译 processor
# ================================
FROM golang:1.21-bookworm AS processor-builder

WORKDIR /src/processor

COPY processor/go.mod processor/go.sum ./
RUN go mod download

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
      bash procps tcpdump netcat-traditional iproute2 net-tools dnsutils psmisc vim-tiny less \
      supervisor tzdata; \
    ln -fs /usr/share/zoneinfo/Asia/Shanghai /etc/localtime; \
    echo "Asia/Shanghai" > /etc/timezone; dpkg-reconfigure -f noninteractive tzdata; \
    rm -rf /var/lib/apt/lists/*

# pmacct binaries + libs
COPY --from=pmacct-builder /usr/local/ /usr/local/
# processor binary
COPY --from=processor-builder /out/processor /usr/local/bin/processor

# dirs
RUN set -eux; \
    mkdir -p /etc/pmacct /var/log/pmacct /var/log/supervisor /var/run/supervisor /opt/pf /var/lib/processor; \
    chmod 755 /var/run/supervisor

# ================================
# 单配置文件模板：/etc/pmacct/pmacct.conf
# 说明：pmacctd 和 nfacctd 都读取同一个文件，各取所需字段
# ================================
RUN cat >/etc/pmacct/pmacct.conf.template <<'EOF'
# ===============================
# pf single config (pmacctd + nfacctd)
# ===============================
daemonize: false

# ---------- exporter (pmacctd + nfprobe) ----------
pcap_interface: __PCAP_IFACE__

# 五元组 + tos（不要加 timestamp_*，否则会碎）
aggregate: src_host, dst_host, src_port, dst_port, proto, tos, tcpflags

plugins: nfprobe
nfprobe_receiver: __NFPROBE_RECEIVER__
nfprobe_version: __NFPROBE_VERSION__
nfprobe_timeouts: __NFPROBE_TIMEOUTS__

# ---------- collector (nfacctd + print) ----------
nfacctd_ip: __NFACCTD_IP__
nfacctd_port: __NFACCTD_PORT__

print_output: csv
print_refresh_time: __PRINT_REFRESH_TIME__
timestamps_since_epoch: true

# 让输出带近似“起止时间”列（timestamp_min/max）
nfacctd_stitching: true

# 需要更稳性能/大窗口/大 key 数时，调大这个（更吃内存）
# print_cache_entries: 262144

# ---------- processor（可注释） ----------
# processor_ftp_host: __PROCESSOR_FTP_HOST__
# processor_ftp_port: __PROCESSOR_FTP_PORT__
# processor_ftp_user: __PROCESSOR_FTP_USER__
# processor_ftp_pass: __PROCESSOR_FTP_PASS__
# processor_ftp_dir: __PROCESSOR_FTP_DIR__
#
# processor_rotate_interval_sec: __PROCESSOR_ROTATE_INTERVAL_SEC__
# processor_rotate_size_mb: __PROCESSOR_ROTATE_SIZE_MB__
# processor_file_prefix: __PROCESSOR_FILE_PREFIX__
#
# processor_upload_interval_sec: __PROCESSOR_UPLOAD_INTERVAL_SEC__
# processor_timezone: __PROCESSOR_TIMEZONE__
#
# processor_status_report_enabled: __PROCESSOR_STATUS_REPORT_ENABLED__
# processor_status_report_url: __PROCESSOR_STATUS_REPORT_URL__
# processor_status_report_interval_sec: __PROCESSOR_STATUS_REPORT_INTERVAL_SEC__
# processor_status_report_uuid: __PROCESSOR_STATUS_REPORT_UUID__
# processor_status_report_file_path: __PROCESSOR_STATUS_REPORT_FILE_PATH__
# processor_status_report_file_max_mb: __PROCESSOR_STATUS_REPORT_FILE_MAX_MB__
# processor_status_report_file_backups: __PROCESSOR_STATUS_REPORT_FILE_BACKUPS__
EOF

# 根据 env 渲染 pmacct.conf（如用户挂载了 /etc/pmacct/pmacct.conf 则不覆盖）
RUN cat >/usr/local/bin/render_pmacct_conf.sh <<'EOF'
#!/usr/bin/env bash
set -eu

CONF="${PMACCT_CONF:-/etc/pmacct/pmacct.conf}"
TPL="${PMACCT_CONF_TEMPLATE:-/etc/pmacct/pmacct.conf.template}"

if [ -s "${CONF}" ]; then
  echo "[render] Using existing config: ${CONF}"
  exit 0
fi

PCAP_IFACE="${PCAP_IFACE:-eth0}"
NFPROBE_RECEIVER="${NFPROBE_RECEIVER:-127.0.0.1:9995}"
NFPROBE_VERSION="${NFPROBE_VERSION:-10}"
NFPROBE_TIMEOUTS="${NFPROBE_TIMEOUTS:-tcp=30:maxlife=60}"

NFACCTD_IP="${NFACCTD_IP:-0.0.0.0}"
NFACCTD_PORT="${NFACCTD_PORT:-9995}"

PRINT_REFRESH_TIME="${PRINT_REFRESH_TIME:-1}"

PROCESSOR_FTP_HOST="${PROCESSOR_FTP_HOST:-}"
PROCESSOR_FTP_PORT="${PROCESSOR_FTP_PORT:-21}"
PROCESSOR_FTP_USER="${PROCESSOR_FTP_USER:-}"
PROCESSOR_FTP_PASS="${PROCESSOR_FTP_PASS:-}"
PROCESSOR_FTP_DIR="${PROCESSOR_FTP_DIR:-/}"
PROCESSOR_ROTATE_INTERVAL_SEC="${PROCESSOR_ROTATE_INTERVAL_SEC:-600}"
PROCESSOR_ROTATE_SIZE_MB="${PROCESSOR_ROTATE_SIZE_MB:-100}"
PROCESSOR_FILE_PREFIX="${PROCESSOR_FILE_PREFIX:-flows_}"
PROCESSOR_UPLOAD_INTERVAL_SEC="${PROCESSOR_UPLOAD_INTERVAL_SEC:-600}"
PROCESSOR_TIMEZONE="${PROCESSOR_TIMEZONE:-Asia/Shanghai}"
PROCESSOR_STATUS_REPORT_ENABLED="${PROCESSOR_STATUS_REPORT_ENABLED:-false}"
PROCESSOR_STATUS_REPORT_URL="${PROCESSOR_STATUS_REPORT_URL:-}"
PROCESSOR_STATUS_REPORT_INTERVAL_SEC="${PROCESSOR_STATUS_REPORT_INTERVAL_SEC:-60}"
PROCESSOR_STATUS_REPORT_UUID="${PROCESSOR_STATUS_REPORT_UUID:-}"
PROCESSOR_STATUS_REPORT_FILE_PATH="${PROCESSOR_STATUS_REPORT_FILE_PATH:-}"
PROCESSOR_STATUS_REPORT_FILE_MAX_MB="${PROCESSOR_STATUS_REPORT_FILE_MAX_MB:-10}"
PROCESSOR_STATUS_REPORT_FILE_BACKUPS="${PROCESSOR_STATUS_REPORT_FILE_BACKUPS:-0}"

echo "[render] Generating ${CONF} from template ${TPL}"
sed \
  -e "s|__PCAP_IFACE__|${PCAP_IFACE}|g" \
  -e "s|__NFPROBE_RECEIVER__|${NFPROBE_RECEIVER}|g" \
  -e "s|__NFPROBE_VERSION__|${NFPROBE_VERSION}|g" \
  -e "s|__NFPROBE_TIMEOUTS__|${NFPROBE_TIMEOUTS}|g" \
  -e "s|__NFACCTD_IP__|${NFACCTD_IP}|g" \
  -e "s|__NFACCTD_PORT__|${NFACCTD_PORT}|g" \
  -e "s|__PRINT_REFRESH_TIME__|${PRINT_REFRESH_TIME}|g" \
  -e "s|__PROCESSOR_FTP_HOST__|${PROCESSOR_FTP_HOST}|g" \
  -e "s|__PROCESSOR_FTP_PORT__|${PROCESSOR_FTP_PORT}|g" \
  -e "s|__PROCESSOR_FTP_USER__|${PROCESSOR_FTP_USER}|g" \
  -e "s|__PROCESSOR_FTP_PASS__|${PROCESSOR_FTP_PASS}|g" \
  -e "s|__PROCESSOR_FTP_DIR__|${PROCESSOR_FTP_DIR}|g" \
  -e "s|__PROCESSOR_ROTATE_INTERVAL_SEC__|${PROCESSOR_ROTATE_INTERVAL_SEC}|g" \
  -e "s|__PROCESSOR_ROTATE_SIZE_MB__|${PROCESSOR_ROTATE_SIZE_MB}|g" \
  -e "s|__PROCESSOR_FILE_PREFIX__|${PROCESSOR_FILE_PREFIX}|g" \
  -e "s|__PROCESSOR_UPLOAD_INTERVAL_SEC__|${PROCESSOR_UPLOAD_INTERVAL_SEC}|g" \
  -e "s|__PROCESSOR_TIMEZONE__|${PROCESSOR_TIMEZONE}|g" \
  -e "s|__PROCESSOR_STATUS_REPORT_ENABLED__|${PROCESSOR_STATUS_REPORT_ENABLED}|g" \
  -e "s|__PROCESSOR_STATUS_REPORT_URL__|${PROCESSOR_STATUS_REPORT_URL}|g" \
  -e "s|__PROCESSOR_STATUS_REPORT_INTERVAL_SEC__|${PROCESSOR_STATUS_REPORT_INTERVAL_SEC}|g" \
  -e "s|__PROCESSOR_STATUS_REPORT_UUID__|${PROCESSOR_STATUS_REPORT_UUID}|g" \
  -e "s|__PROCESSOR_STATUS_REPORT_FILE_PATH__|${PROCESSOR_STATUS_REPORT_FILE_PATH}|g" \
  -e "s|__PROCESSOR_STATUS_REPORT_FILE_MAX_MB__|${PROCESSOR_STATUS_REPORT_FILE_MAX_MB}|g" \
  -e "s|__PROCESSOR_STATUS_REPORT_FILE_BACKUPS__|${PROCESSOR_STATUS_REPORT_FILE_BACKUPS}|g" \
  "${TPL}" > "${CONF}"

echo "[render] Done. Preview:"
head -n 120 "${CONF}"
EOF

# exporter 启动脚本
RUN cat >/usr/local/bin/start_exporter.sh <<'EOF'
#!/usr/bin/env bash
set -eu
/usr/local/bin/render_pmacct_conf.sh
CONF="${PMACCT_CONF:-/etc/pmacct/pmacct.conf}"
RUNTIME_DIR="/var/run/pmacct"
FILTERED_CONF="${RUNTIME_DIR}/pmacctd.conf"
mkdir -p "${RUNTIME_DIR}"

# 过滤掉 processor_* 配置，避免 pmacctd 报未知项
awk '
  /^[[:space:]]*processor_/ {next}
  /^[[:space:]]*#[[:space:]]*processor_/ {next}
  {print}
' "${CONF}" > "${FILTERED_CONF}"

echo "[exporter] conf=${FILTERED_CONF}"
exec /usr/local/sbin/pmacctd -f "${FILTERED_CONF}"
EOF

# collector 启动脚本（输出到 stdout，由 processor 接收）
RUN cat >/usr/local/bin/start_collector.sh <<'EOF'
#!/usr/bin/env bash
set -eu
/usr/local/bin/render_pmacct_conf.sh
CONF="${PMACCT_CONF:-/etc/pmacct/pmacct.conf}"
RUNTIME_DIR="/var/run/pmacct"
FILTERED_CONF="${RUNTIME_DIR}/nfacctd.conf"
PROCESSOR_BIN="${PROCESSOR_BIN:-/usr/local/bin/processor}"
PROCESSOR_CONFIG="${PROCESSOR_CONFIG:-/etc/pmacct/pmacct.conf}"
PROCESSOR_DATA_DIR="${PROCESSOR_DATA_DIR:-/var/lib/processor}"
PROCESSOR_LOG_LEVEL="${PROCESSOR_LOG_LEVEL:-info}"

if [ ! -x "${PROCESSOR_BIN}" ]; then
  echo "[collector] processor not found or not executable: ${PROCESSOR_BIN}"
  exit 1
fi
mkdir -p "${RUNTIME_DIR}" "${PROCESSOR_DATA_DIR}"
echo "[collector] conf=${CONF}"

# nfacctd 不支持 nfprobe 插件；过滤 processor_* 与 nfprobe_*，并强制 plugins: print
awk '
  BEGIN {plugins_set=0}
  /^[[:space:]]*plugins:/ {print "plugins: print"; plugins_set=1; next}
  /^[[:space:]]*nfprobe_/ {next}
  /^[[:space:]]*processor_/ {next}
  /^[[:space:]]*#[[:space:]]*processor_/ {next}
  {print}
  END {if (!plugins_set) print "plugins: print"}
' "${CONF}" > "${FILTERED_CONF}"

echo "[collector] nfacctd_conf=${FILTERED_CONF}"
echo "[collector] processor=${PROCESSOR_BIN}"
echo "[collector] processor_config=${PROCESSOR_CONFIG}"
echo "[collector] processor_data_dir=${PROCESSOR_DATA_DIR}"
echo "[collector] processor_log_level=${PROCESSOR_LOG_LEVEL}"
set -o pipefail
/usr/local/sbin/nfacctd -f "${FILTERED_CONF}" | \
  "${PROCESSOR_BIN}" -config "${PROCESSOR_CONFIG}" -data-dir "${PROCESSOR_DATA_DIR}" -log-level "${PROCESSOR_LOG_LEVEL}"
EOF

RUN sed -i 's/\r$//' /usr/local/bin/render_pmacct_conf.sh /usr/local/bin/start_exporter.sh /usr/local/bin/start_collector.sh && \
    chmod +x /usr/local/bin/render_pmacct_conf.sh /usr/local/bin/start_exporter.sh /usr/local/bin/start_collector.sh

# ================================
# supervisor：默认同时起 exporter + collector
# 通过环境变量 ENABLE_COLLECTOR / ENABLE_EXPORTER 控制启停
# ================================
RUN cat >/usr/local/bin/supervisor_entry.sh <<'EOF'
#!/usr/bin/env bash
set -eu

ENABLE_EXPORTER="${ENABLE_EXPORTER:-true}"
ENABLE_COLLECTOR="${ENABLE_COLLECTOR:-true}"

# 动态生成 supervisord 程序段，方便按 env 开关
cat >/etc/supervisor/supervisord.conf <<'CONF'
[unix_http_server]
file=/var/run/supervisor/supervisor.sock
chmod=0700

[supervisord]
nodaemon=true
logfile=/dev/stdout
logfile_maxbytes=0
logfile_backups=0
pidfile=/var/run/supervisor/supervisord.pid
user=root

[rpcinterface:supervisor]
supervisor.rpcinterface_factory = supervisor.rpcinterface:make_main_rpcinterface

[supervisorctl]
serverurl=unix:///var/run/supervisor/supervisor.sock
CONF

if [ "${ENABLE_COLLECTOR}" = "true" ]; then
  cat >>/etc/supervisor/supervisord.conf <<'CONF'

[program:collector]
command=/bin/bash /usr/local/bin/start_collector.sh
autorestart=true
startsecs=2
startretries=10
priority=100
stdout_logfile=/var/log/supervisor/collector.log
stdout_logfile_maxbytes=20MB
stdout_logfile_backups=5
stderr_logfile=/dev/stdout
stderr_logfile_maxbytes=0
stderr_logfile_backups=0
CONF
else
  echo "[entry] collector disabled"
fi

if [ "${ENABLE_EXPORTER}" = "true" ]; then
  cat >>/etc/supervisor/supervisord.conf <<'CONF'

[program:exporter]
command=/bin/bash /usr/local/bin/start_exporter.sh
autorestart=true
startsecs=2
startretries=10
priority=200
stdout_logfile=/var/log/supervisor/exporter.log
stdout_logfile_maxbytes=20MB
stdout_logfile_backups=5
stderr_logfile=/dev/stdout
stderr_logfile_maxbytes=0
stderr_logfile_backups=0
CONF
else
  echo "[entry] exporter disabled"
fi

exec /usr/bin/supervisord -c /etc/supervisor/supervisord.conf
EOF

RUN sed -i 's/\r$//' /usr/local/bin/supervisor_entry.sh && chmod +x /usr/local/bin/supervisor_entry.sh

WORKDIR /opt/pf
ENTRYPOINT ["/usr/local/bin/supervisor_entry.sh"]
CMD []
