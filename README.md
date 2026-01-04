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
- 日志：容器 stdout/stderr（含 pmacct 与 processor 输出）

## 配置

统一使用 `pmacct.conf`（pmacct 配置 + processor 配置）。  
`processor` 使用 `processor_*` 格式的 `key: value` 行，建议保留注释避免 pmacct 报未知项。

示例片段（加入到 `pmacct.conf` 末尾即可）：

```conf
# processor 配置（可注释）
# processor_ftp_host: 10.0.0.10
# processor_ftp_port: 21
# processor_ftp_user: ftpuser
# processor_ftp_pass: ftppass
# processor_ftp_dir: /data/areaA
#
# processor_rotate_interval_sec: 600
# processor_rotate_size_mb: 100
# processor_file_prefix: flows_
#
# processor_upload_interval_sec: 600
# processor_timezone: Asia/Shanghai
#
# processor_status_report_enabled: false
# processor_status_report_url: http://127.0.0.1:8080/api/uploadStatus
# processor_status_report_interval_sec: 60
# processor_status_report_uuid:
# processor_status_report_file_path:
# processor_status_report_file_max_mb: 10
# processor_status_report_file_backups: 0
```

## 构建镜像

```bash
docker build -t pf:latest .
```

## 运行（推荐）

```bash
docker run -d --name pf \
  --network host --privileged \
  -e PCAP_IFACE=eth4 \
  -e PROCESSOR_CONFIG=/etc/pmacct/pmacct.conf \
  -e PROCESSOR_DATA_DIR=/var/lib/processor \
  -v /path/to/pmacct.conf:/etc/pmacct/pmacct.conf:ro \
  -v /path/to/data:/var/lib/processor \
  -v "$PWD/log:/var/log/pmacct" \
  pf:latest
```

## 最精简运行

仅保留必须挂载（配置 + 数据目录）：

```bash
docker run -d --name pf \
  --network host --privileged \
  -v /mnt/d/projects/pf/pmacct.conf:/etc/pmacct/pmacct.conf:ro \
  -v /path/to/data:/var/lib/processor \
  pf:latest
```

## 常用环境变量

- `PCAP_IFACE`：抓包网卡（默认 `eth0`）
- `NFPROBE_RECEIVER`：IPFIX 接收端（默认 `127.0.0.1:9995`）
- `ENABLE_EXPORTER`：是否启用 pmacctd（默认 `true`）
- `ENABLE_COLLECTOR`：是否启用 nfacctd + processor（默认 `true`）
- `PROCESSOR_CONFIG`：pmacct.conf 路径（默认 `/etc/pmacct/pmacct.conf`）
- `PROCESSOR_DATA_DIR`：processor 数据目录（默认 `/var/lib/processor`）
- `PROCESSOR_LOG_LEVEL`：processor 日志级别（默认 `info`）

## 说明

- 如需只跑 exporter 或 collector，设置 `ENABLE_EXPORTER=false` 或 `ENABLE_COLLECTOR=false`
- `processor` 仅从 stdin 读取，不会写入 `/var/log/pmacct`
- 若 `pmacct.conf` 中未配置 `processor_*`，`processor` 会启动失败并退出
