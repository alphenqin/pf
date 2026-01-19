# pf

pf 是一个一体化的流量采集与处理容器，包含：
- **pmacctd**：抓包并导出 IPFIX
- **nfacctd**：接收 IPFIX 并输出 CSV
- **processor**：从 stdin 读取 CSV，滚动写入 gzip 文件并上传 FTP

## 运行流程

1) `pmacctd` 抓网卡流量，使用 `nfprobe` 导出 IPFIX 到本机 `nfacctd`  
2) `nfacctd` 把 IPFIX 转成 CSV 输出到 stdout  
3) `processor` 从 stdout 接收 CSV，生成 `.csv.gz` 文件并按周期上传 FTP

## 结果产出

- 本地数据目录：生成滚动的 `*.csv.gz` 文件（写入中为 `.part`）
- FTP：按 `processor_*` 配置上传到目标目录
- 日志：容器 stdout/stderr（含 pmacct 与 processor 输出），同时落盘到 `/var/log/pmacct/*.log`

## 配置

统一使用 `pmacct.conf`（pmacct 配置 + processor 配置）。  
`processor` 使用 `processor_*` 格式的 `key: value` 行（注释行不会解析）。  
镜像内默认包含仓库里的 `pmacct.conf`；若运行时挂载同路径文件，会覆盖默认配置。

示例片段（加入到 `pmacct.conf` 末尾即可，**不要注释**）：

```conf
# processor 配置
processor_ftp_host: 10.0.0.10
processor_ftp_port: 21
processor_ftp_user: ftpuser
processor_ftp_pass: ftppass
processor_ftp_dir: /data/areaA
processor_ftp_timeout: 300

processor_rotate_interval_sec: 600
processor_rotate_size_mb: 100
processor_file_prefix: flows_

processor_upload_interval_sec: 600
processor_timezone: Asia/Shanghai
processor_debug_print_interval: 0
processor_ingest_chan_capacity: 10000
processor_ingest_chan_timeout_ms: 100
# 超时设为 0 表示不丢弃（阻塞等待写入）

processor_status_report_enabled: false
processor_status_report_url: http://127.0.0.1:8080/api/uploadStatus
processor_status_report_interval_sec: 60
processor_status_report_uuid:
processor_status_report_file_path:
processor_status_report_file_max_mb: 10
processor_status_report_file_backups: 0

# 诊断采集（宿主机日志 + 容器进程资源指标）
processor_diag_enabled: false
processor_diag_interval_sec: 600
```

## 诊断采集说明

- 宿主机脚本产出：
  - `syslog_*.log`（原始系统日志增量片段）
  - `env_*.json`（环境信息快照）
    - 字段结构与 `system_monitor` 保持一致：
      - `disk_info[]`
      - `cpu_info`
      - `mem_info`
      - `host_info`
      - `gpu_info[]`
      - `disk_io[]`
      - `ip`
      - `exec_time`
- 容器内读取固定路径：
  - 宿主机日志：`/var/lib/processor/log`
  - 输出目录：`/var/lib/processor`（与 CSV 同级）
  并在容器内采集以下进程资源指标（CPU/内存/IO）：`pmacctd / nfacctd / processor`，当系统日志 + 环境信息 + 进程指标齐全时生成结构化合并文件：
  - `diag_<host>_<ts>.json.gz`（JSON Lines，按时间排序）
  - 统一字段格式：`ts, host, src, level, msg, payload`
  - `payload` 存放各类型特有字段（包含 CPU/内存/IO 等）
- 日志文件输出到数据目录根目录，并上传至 FTP 的 `processor_ftp_dir`；打包成功后会清理已处理的宿主机日志与环境文件

## 诊断采集与上传流程（syslog / env / proc）

### 1) 宿主机采集（start.sh）

- **syslog 增量截取**
  - 读取 `/var/log/syslog` 或 `/var/log/messages`。
  - 使用 inode + offset（`${OUT_DIR}/.${BASE_NAME}.state`）记录上次读取位置。
  - 新增内容写入 `${OUT_DIR}/syslog_<host>_<ts>.log`。
- **env 快照**
  - 使用 `system_monitor` 口径生成 JSON（字段结构与 `system_monitor` 的 `sysinfo` 一致）。
  - 用 `${OUT_DIR}/.env.state` 存 hash，内容未变化则不落盘。
  - 变化时写入 `${OUT_DIR}/env_<host>_<ts>.json`。
- **目录落点**
  - `${OUT_DIR}` 默认是 `${PF_DATA_DIR}/log`，容器内固定挂载到 `/var/lib/processor/log`。

### 宿主机 start.sh 运行流程（当前实现）

1) **自动写入 cron（每 5 分钟）**  
   - 运行 `start.sh` 时会检查是否已存在以下任务，若不存在则写入：  
     `*/5 * * * * PF_DATA_DIR=<data_dir> /mnt/d/projects/pf/start.sh >/dev/null 2>&1`
2) **系统日志增量采集**  
   - 从 `/var/log/syslog` 或 `/var/log/messages` 读取新增内容  
   - 写入 `${OUT_DIR}/syslog_<host>_<ts>.log`
3) **环境信息采集（system_monitor 口径）**  
   - 调用根目录的 `system_monitor_linux_amd64` 或 `system_monitor_linux_arm64`  
   - 内置配置：Kafka/Redis 关闭、日志输出到 stdout  
   - 从 stdout 提取 `bytesData info: {…}` JSON  
   - JSON 变化时写入 `${OUT_DIR}/env_<host>_<ts>.json`

### 2) 容器内诊断合并（processor / diag）

- **syslog 解析**
  - 扫描 `/var/lib/processor/log/syslog_*.log`，按文件名排序。
  - 已处理文件记录在 `data/diag/syslog_processed.list`，避免重复。
  - 解析 RFC5424 / RFC3164；`host` 为日志行内主机名，若为空回退到本机 FQDN。
  - 生成标准化记录：`ts/host/src/level/msg/payload`。
- **env 读取**
  - 读取 `/var/lib/processor/log` 下最新 `env_*.json`。
  - 记录 `data/diag/env_latest.state`，仅当文件变化时更新。
  - 组装 `env` 记录：`src=env`，`host` 若缺失则使用本机 FQDN。
- **proc 指标采集**
  - 读取 `/proc`，针对 `pmacctd/nfacctd/processor` 采样。
  - 计算 CPU 使用率（delta jiffies），并采集内存/IO/FD 等。
  - 记录 `src=proc`，`level=info`。
- **合并与输出**
  - 当 syslog/proc/env 任一有数据时，写入 `diag_<host>_<ts>.json.gz`。
  - JSON Lines 格式，按 `ts` 排序。
  - 写入成功后清理已处理的 syslog/env 源文件。

### 3) 上传流程（uploader）

- 扫描数据目录（`/var/lib/processor`），上传：
  - `*.csv.gz`（流量数据）
  - `*.json.gz`（诊断数据）
- **上传逻辑**
  - 每次扫描先清理远端残留 `.tmp` 文件。
  - 上传时先传到 `filename.tmp`，校验大小后 `Rename` 成正式文件。
  - 远端同名且大小一致则跳过。
  - 成功后删除本地文件；失败保留，等待下次扫描重试。

## 从容器拷出宿主机采集脚本

镜像内内置 `/usr/local/bin/start.sh`，可拷出到宿主机执行：

```bash
docker cp <container>:/usr/local/bin/start.sh /mnt/d/projects/pf/start.sh
docker cp <container>:/usr/local/bin/system_monitor_linux_amd64 /mnt/d/projects/pf/system_monitor_linux_amd64
docker cp <container>:/usr/local/bin/system_monitor_linux_arm64 /mnt/d/projects/pf/system_monitor_linux_arm64
chmod +x /mnt/d/projects/pf/start.sh
```

宿主机运行示例（会自动写入每 5 分钟的 cron 任务）：

```bash
PF_DATA_DIR=/mnt/d/projects/pf/data ./start.sh
```

说明：
- `start.sh` 会自动写入 cron，无需手动配置 crontab。
- 如果不用 `./` 或绝对路径，需保证 `start.sh` 在 PATH 中。

## 构建镜像

默认使用预编译的 pmacct 包（`pmacct-usrlocal-amd64.tar` / `pmacct-usrlocal-arm64.tar`），
不编译源码，速度更快。需要源码编译时再用参数开启。

```bash
docker build -t pf:latest .
```

多平台（推荐，自动按架构选预编译包）：

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t pf:latest .
```

如需源码编译（耗时较长）：

```bash
docker buildx build \
  --build-arg PMACCT_MODE=source \
  --build-arg PMACCT_VERSION=1.7.9 \
  -t pf:latest .
```

## 运行（推荐）

```bash
docker run -d --name pf \
  --network host --privileged \
  -e PROCESSOR_CONFIG=/etc/pmacct/pmacct.conf \
  -e PROCESSOR_DATA_DIR=/var/lib/processor \
  -v /path/to/pmacct.conf:/etc/pmacct/pmacct.conf:ro \
  -v /path/to/data:/var/lib/processor \
  pf:latest
```

## 最精简运行

仅保留必须挂载（配置 + 数据目录）：

```bash
docker run -d --name pf \
  --network host --privileged \
  -v /mnt/d/projects/pf/pmacct.conf:/etc/pmacct/pmacct.conf:ro \
  -v /mnt/d/projects/pf/data:/var/lib/processor \
  pf:latest
```

## 常用环境变量

- `PROCESSOR_CONFIG`：pmacct.conf 路径（默认 `/etc/pmacct/pmacct.conf`）
- `PROCESSOR_DATA_DIR`：processor 数据目录（默认 `/var/lib/processor`）
- `PROCESSOR_LOG_LEVEL`：processor 日志级别（默认 `info`）

说明：pmacct 的抓包/导出/采集配置请直接写在 `pmacct.conf` 中。

## 说明

- `pmacctd/nfacctd/processor` 日志会写入 `/var/log/pmacct/*.log`
- 若 `pmacct.conf` 中未配置 `processor_*`，`processor` 会启动失败并退出
