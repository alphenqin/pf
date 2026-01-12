#!/usr/bin/env bash
set -eu

BASE_DIR="$(cd "$(dirname "$0")" && pwd)"
DATA_DIR="${PF_DATA_DIR:-${1:-${BASE_DIR}/data}}"
OUT_DIR="${DATA_DIR}/log"
HOST="$(hostname -s 2>/dev/null || hostname)"
TS="$(date -u +%Y%m%dT%H%M%SZ)"

mkdir -p "${OUT_DIR}"

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
    tail -c +"$((PREV_OFFSET + 1))" "${SYSLOG_FILE}" | gzip > "${OUT_DIR}/syslog_${HOST}_${TS}_v1.log.gz"
    printf '%s %s\n' "${CUR_INODE}" "${CUR_SIZE}" > "${STATE_FILE}"
  fi
fi

# ---- 环境信息（宿主机快照）----
ENV_JSON="$(
  printf '{'
  printf '"ts":"%s",' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '"host":"%s",' "${HOST}"
  if [ -f /etc/os-release ]; then
    OS_LINE="$(tr -d '\n' </etc/os-release | sed 's/"/\\"/g')"
    printf '"os":"%s",' "${OS_LINE}"
  else
    printf '"os":"unknown",'
  fi
  printf '"kernel":"%s",' "$(uname -r)"
  if [ -r /proc/uptime ]; then
    printf '"uptime_sec":%s,' "$(cut -d. -f1 /proc/uptime)"
  else
    printf '"uptime_sec":0,'
  fi
  printf '"cpu_cores":%s,' "$(nproc 2>/dev/null || echo 0)"
  if [ -r /proc/meminfo ]; then
    printf '"mem_total_kb":%s,' "$(awk '/MemTotal/ {print $2}' /proc/meminfo)"
    printf '"mem_avail_kb":%s,' "$(awk '/MemAvailable/ {print $2}' /proc/meminfo)"
  else
    printf '"mem_total_kb":0,"mem_avail_kb":0,'
  fi
  if command -v ip >/dev/null 2>&1; then
    IP_LIST="$(ip -o -4 addr show 2>/dev/null | awk '{print $4}' | sed 's#/.*##' | paste -sd ',' -)"
    printf '"ip":"%s",' "${IP_LIST}"
  else
    printf '"ip":"",'
  fi
  DISK_JSON="$(df -P -k 2>/dev/null | awk 'NR>1{printf "{\"mount\":\"%s\",\"total_kb\":%s,\"used_kb\":%s,\"avail_kb\":%s},", $6,$2,$3,$4}' | sed 's/,$//')"
  printf '"disk":[%s]' "${DISK_JSON}"
  printf '}\n'
)"

ENV_HASH_FILE="${OUT_DIR}/.env.state"
ENV_HASH="$(printf '%s' "${ENV_JSON}" | sha256sum | awk '{print $1}')"
PREV_ENV_HASH=""
if [ -s "${ENV_HASH_FILE}" ]; then
  PREV_ENV_HASH="$(cat "${ENV_HASH_FILE}")"
fi
if [ "${ENV_HASH}" != "${PREV_ENV_HASH}" ]; then
  printf '%s' "${ENV_JSON}" | gzip > "${OUT_DIR}/env_${HOST}_${TS}_v1.json.gz"
  printf '%s\n' "${ENV_HASH}" > "${ENV_HASH_FILE}"
fi
