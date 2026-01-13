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
    - 字段：`ts, host, os, kernel, uptime_sec, load1, load5, load15, cpu_cores, mem_total_kb, mem_avail_kb, ip, disk`
- 容器内读取固定路径：
  - 宿主机日志：`/var/lib/processor/log`
  - 输出目录：`/var/lib/processor`（与 CSV 同级）
  并在容器内采集以下进程资源指标（CPU/内存/IO）：`pmacctd / nfacctd / processor`，当系统日志 + 环境信息 + 进程指标齐全时生成结构化合并文件：
  - `diag_<host>_<ts>.json.gz`（JSON Lines，按时间排序）
  - 统一字段格式：`ts, host, src, level, msg, payload`
  - `payload` 存放各类型特有字段（包含 CPU/内存/IO 等）
- 日志文件输出到数据目录根目录，并上传至 FTP 的 `processor_ftp_dir`；打包成功后会清理已处理的宿主机日志与环境文件

## 从容器拷出宿主机采集脚本

镜像内内置 `/usr/local/bin/start.sh`，可拷出到宿主机执行：

```bash
docker cp <container>:/usr/local/bin/start.sh /mnt/d/projects/pf/start.sh
chmod +x /mnt/d/projects/pf/start.sh
```

宿主机运行示例：

```bash
PF_DATA_DIR=/mnt/d/projects/pf/data /mnt/d/projects/pf/start.sh
```

定时运行示例（每 5 分钟）：

```bash
*/5 * * * * PF_DATA_DIR=/mnt/d/projects/pf/data /mnt/d/projects/pf/start.sh >/dev/null 2>&1
```

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
