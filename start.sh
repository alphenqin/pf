#!/usr/bin/env bash
set -eu

BASE_DIR="$(cd "$(dirname "$0")" && pwd)"
DATA_DIR="${PF_DATA_DIR:-${1:-${BASE_DIR}/data}}"
OUT_DIR="${DATA_DIR}/log"
HOST="$(hostname -f 2>/dev/null || uname -n 2>/dev/null || hostname)"
TS="$(TZ=Asia/Shanghai date +%Y%m%dT%H%M%S%z)"

mkdir -p "${OUT_DIR}"

CRON_SCHEDULE="*/5 * * * *"
CRON_CMD="PF_DATA_DIR=${DATA_DIR} ${BASE_DIR}/start.sh >/dev/null 2>&1"
if ! crontab -l 2>/dev/null | grep -Fq "${CRON_CMD}"; then
  TMP_CRON="$(mktemp)"
  crontab -l 2>/dev/null > "${TMP_CRON}" || true
  printf "%s %s\n" "${CRON_SCHEDULE}" "${CRON_CMD}" >> "${TMP_CRON}"
  crontab "${TMP_CRON}"
  rm -f "${TMP_CRON}"
  echo "cron installed: ${CRON_SCHEDULE} ${CRON_CMD}"
fi

# ---- 系统日志（宿主机文件）----
SYSLOG_FILE=""
if [ -f /var/log/syslog ]; then
  SYSLOG_FILE="/var/log/syslog"
elif [ -f /var/log/messages ]; then
  SYSLOG_FILE="/var/log/messages"
fi

if [ -n "${SYSLOG_FILE}" ]; then
  BASE_NAME="$(basename "${SYSLOG_FILE}")"
  STATE_FILE="${OUT_DIR}/.${BASE_NAME}.state"
  CUR_INODE="$(stat -c %i "${SYSLOG_FILE}" 2>/dev/null || echo 0)"
  CUR_SIZE="$(stat -c %s "${SYSLOG_FILE}" 2>/dev/null || echo 0)"
  PREV_INODE=0
  PREV_OFFSET=0
  if [ -s "${STATE_FILE}" ]; then
    read -r PREV_INODE PREV_OFFSET < "${STATE_FILE}" || true
  fi
  if [ "${PREV_INODE}" != "${CUR_INODE}" ] || [ "${PREV_OFFSET}" -gt "${CUR_SIZE}" ]; then
    PREV_OFFSET=0
  fi
  NEW_BYTES=$((CUR_SIZE - PREV_OFFSET))
  if [ "${NEW_BYTES}" -gt 0 ]; then
    tail -c +"$((PREV_OFFSET + 1))" "${SYSLOG_FILE}" > "${OUT_DIR}/syslog_${HOST}_${TS}.log"
    printf '%s %s\n' "${CUR_INODE}" "${CUR_SIZE}" > "${STATE_FILE}"
  fi
fi

# ---- 环境信息（宿主机快照，口径对齐 system_monitor）----
SM_BIN=""
case "$(uname -m)" in
  x86_64|amd64) SM_BIN="${BASE_DIR}/system_monitor_linux_amd64" ;;
  aarch64|arm64) SM_BIN="${BASE_DIR}/system_monitor_linux_arm64" ;;
  *) SM_BIN="" ;;
esac

ENV_JSON=""
if [ -n "${SM_BIN}" ] && [ -x "${SM_BIN}" ]; then
  SM_TMP_DIR="$(mktemp -d)"
  SM_CFG="${SM_TMP_DIR}/config.yaml"
  cat > "${SM_CFG}" <<'EOF'
app_name: system_monitor
version: v1
log_level: info
model: debug
max_procs: 8

file_log:
  log_path: /dev/stdout
  log_level: info
  max_size: 50
  max_backups: 5
  max_ages: 60
  compress: false

kfk_config:
  open: 0
  bulk_number: 100
  max_records: 1
  timeout_ms: 120000
  server_list: ["0.0.0.0:9093"]
  topic: "system_monitor"

redis_config:
  open: 0
  addr: "127.0.0.1:6379"
  db: 0
  key: "system_monitor"
  password: ""
EOF

  SM_OUT="$("${SM_BIN}" "${SM_CFG}" 2>/dev/null || true)"
  ENV_JSON="$(printf '%s\n' "${SM_OUT}" | sed -n 's/.*bytesData info: //p' | tail -n 1)"
  rm -rf "${SM_TMP_DIR}"
else
  echo "system_monitor binary not found or not executable: ${SM_BIN}" >&2
fi

if [ -z "${ENV_JSON}" ]; then
  echo "system_monitor output missing, env snapshot will be empty JSON" >&2
  ENV_JSON='{}'
fi

ENV_HASH_FILE="${OUT_DIR}/.env.state"
ENV_HASH="$(printf '%s' "${ENV_JSON}" | sha256sum | awk '{print $1}')"
PREV_ENV_HASH=""
if [ -s "${ENV_HASH_FILE}" ]; then
  PREV_ENV_HASH="$(cat "${ENV_HASH_FILE}")"
fi
if [ "${ENV_HASH}" != "${PREV_ENV_HASH}" ]; then
  printf '%s' "${ENV_JSON}" > "${OUT_DIR}/env_${HOST}_${TS}.json"
  printf '%s\n' "${ENV_HASH}" > "${ENV_HASH_FILE}"
fi
