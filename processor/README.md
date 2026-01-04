# processor

processor 是一个集成了 pmacct（pmacctd/nfacctd）和 processor 的完整流量采集与 FTP 上传解决方案。

## 项目简介

processor 通过 Docker 容器化部署，实现了从网络流量采集到数据上传的完整流程：

1. **pmacctd**：从网络接口抓取流量，生成 IPFIX 流记录
2. **nfacctd**：接收 IPFIX 并输出 CSV 文本格式
3. **processor**：接收 CSV 数据，滚动写入本地 gzip 压缩文件，并定时上传到 FTP 服务器

### processor 组件

processor 是项目的核心组件，使用 Go 语言开发，负责：
- 从标准输入（管道）读取 CSV 数据
- 滚动写入本地 gzip 压缩文件（按时间和大小策略）
- 定时扫描目录，自动上传已完成的压缩文件到 FTP 服务器
- 所有配置通过 `pmacct.conf` 配置文件统一管理

## 功能特性

### 整体方案
- **一体化部署**：pmacctd + nfacctd + processor 集成在一个 Docker 镜像中
- **开箱即用**：容器启动后自动运行完整的流量采集和上传流程
- **配置统一**：所有组件配置集中在 `pmacct.conf` 文件中

### processor 组件
- **滚动文件写入**：按时间间隔和文件大小自动滚动生成新的压缩文件
- **FTP 自动上传**：定时扫描本地目录，自动上传已完成的压缩文件到 FTP 服务器
- **状态上报（可选）**：支持按周期 HTTP 上报运行统计
- **配置驱动**：所有参数通过 `pmacct.conf` 配置文件中的 `processor_*` 配置项管理
- **优雅关闭**：支持信号处理，确保数据不丢失

## 项目结构

```
processor/
├── cmd/
│   └── processor/          # 主程序入口
│       └── main.go
├── internal/
│   ├── config/            # 配置解析模块
│   │   └── config.go
│   ├── writer/            # 文件滚动写入模块
│   │   └── writer.go
│   └── uploader/          # FTP 上传模块
│       └── uploader.go
├── Dockerfile             # Docker 构建文件
├── go.mod
├── go.sum
└── README.md
```

## 构建

需要 Go 1.21+

```bash
# 拉取依赖
go mod tidy

# 编译（当前平台）
go build -o processor ./cmd/processor

# 编译 Linux amd64（用于 Docker）
GOOS=linux GOARCH=amd64 go build -o processor ./cmd/processor
```

## 使用方法

### 命令行参数

- `-config string`：**必填**。pmacct 配置文件路径（`pmacct.conf`），例如：`/etc/pmacct/pmacct.conf`
- `-data-dir string`：**必填**。本地缓存目录，用来存放滚动生成的压缩文件
- `-log-level string`：可选。日志级别：`debug|info|warn|error`，默认 `info`

### 配置文件格式

所有 FTP 和滚动参数都配置在 `pmacct.conf` 文件的 `processor_*` 配置项中（注释行不会被解析）： 

```conf
# pmacct 配置（略）
pcap_interface: eth0

# processor 配置（不要注释，否则不会生效）
processor_ftp_host: 10.0.0.10
processor_ftp_port: 21
processor_ftp_user: ftpuser
processor_ftp_pass: ftppass
processor_ftp_dir: /data/areaA

processor_rotate_interval_sec: 600
processor_rotate_size_mb: 100
processor_file_prefix: flows_

processor_upload_interval_sec: 600
processor_timezone: Asia/Shanghai

processor_status_report_enabled: false
processor_status_report_url: http://127.0.0.1:8080/api/uploadStatus
processor_status_report_interval_sec: 60
processor_status_report_uuid:
processor_status_report_file_path:
processor_status_report_file_max_mb: 10
processor_status_report_file_backups: 0
```

### 运行方式

程序从**标准输入（stdin）**读取 CSV 数据，支持以下使用场景：

1. **管道输入**（推荐）：与其他工具通过管道连接
2. **重定向输入**：从文件重定向到标准输入
3. **Docker 容器内**：作为 nfacctd 的下游处理程序

### 运行示例

#### 1. 管道模式（与 nfacctd 集成）

```bash
/usr/local/sbin/nfacctd -f /etc/pmacct/pmacct.conf | \
  ./processor -config /etc/pmacct/pmacct.conf -data-dir /data
```

#### 2. 文件重定向模式

```bash
./processor -config /etc/pmacct/pmacct.conf -data-dir /data < data.csv
```

#### 3. Docker 容器内使用

参考 Dockerfile 中的集成方式，容器会自动启动 pipeline：

```bash
/usr/local/sbin/nfacctd -f /etc/pmacct/pmacct.conf | \
  /usr/local/bin/processor -config /etc/pmacct/pmacct.conf -data-dir /data
```

## Docker 使用

### 准备构建上下文

Dockerfile 需要从构建上下文中复制已编译好的 `processor` 二进制文件。构建前需要准备：

1. **编译 processor 二进制文件（Linux amd64）**：
   ```bash
   # 在项目根目录执行
   GOOS=linux GOARCH=amd64 go build -o processor ./cmd/processor
   ```

2. **准备构建目录**：
   将编译好的 `processor` 二进制文件和 `Dockerfile` 放在同一目录中：
   ```bash
   # 创建构建目录
   mkdir docker-build
   
   # 复制必要文件
   cp processor docker-build/
   cp Dockerfile docker-build/
   ```

### 构建镜像

在包含 `processor` 和 `Dockerfile` 的目录中执行：

```bash
cd docker-build
docker build -t processor:alpha1 .
```

**完整构建流程示例**：

```bash
# 1. 在项目根目录编译二进制
GOOS=linux GOARCH=amd64 go build -o processor ./cmd/processor

# 2. 创建构建目录并复制文件
mkdir -p docker-build
cp processor docker-build/
cp Dockerfile docker-build/

# 3. 进入构建目录并构建镜像
cd docker-build
docker build -t processor:alpha1 .

# 4. 验证镜像
docker images | grep processor
```

### 运行容器

```bash
docker run -d \
  --name processor \
  --net=host \
  --cap-add NET_ADMIN \
  --user root \
  -e PROCESSOR_CONFIG=/etc/pmacct/pmacct.conf \
  -v /path/to/pmacct.conf:/etc/pmacct/pmacct.conf:ro \
  -v /path/to/data:/data \
  processor:alpha1
```

**参数说明：**
- `--net=host`：使用主机网络模式（pmacct 需要抓包）
- `--cap-add NET_ADMIN`：授予网络管理权限（pmacct 需要）
- `-v /path/to/pmacct.conf:/etc/pmacct/pmacct.conf:ro`：挂载 pmacct 配置文件（只读）
- `-v /path/to/data:/data`：挂载数据目录（用于存储滚动文件）

## 文件命名规则

生成的文件命名格式：`{file_prefix}{YYYYMMDD_HHMMSS}_{index}.csv.gz`

例如：
- `flows_20251127_102030_000.csv.gz`
- `flows_20251127_102030_001.csv.gz`

写入时使用 `.part` 后缀，完成后自动重命名为 `.csv.gz`：
- 写入中：`flows_20251127_102030_000.part`
- 完成后：`flows_20251127_102030_000.csv.gz`

## 滚动策略

文件会在以下任一条件满足时滚动：

1. **时间间隔**：自文件创建起超过 `rotate_interval_sec` 秒
2. **文件大小**：已写入的原始字节数（未压缩）超过 `rotate_size_mb` MB

## FTP 上传策略

- 定时扫描：每隔 `upload_interval_sec` 秒扫描一次数据目录
- 只上传已完成文件：仅处理 `.csv.gz` 后缀的文件，忽略 `.part` 文件
- 上传成功后删除：本地文件上传成功后自动删除
- 失败重试：上传失败的文件会在下一个周期再次尝试

## 日志输出

所有日志输出到 `stderr`，包含时间戳：

```
2025/11/27 10:00:00 [INFO] 配置加载成功: FTP=10.0.0.10:21, 滚动间隔=600s, 滚动大小=100MB, 上传间隔=600s
2025/11/27 10:00:00 [INFO] 数据目录已就绪: /data
2025/11/27 10:00:00 [INFO] FTP 上传器已启动，上传间隔: 600s
2025/11/27 10:10:00 [INFO] rotate file: /data/flows_20251127_100000_000.csv.gz size=98.3MB duration=600s
2025/11/27 10:10:05 [INFO] FTP 上传成功并删除本地文件: flows_20251127_100000_000.csv.gz
2025/11/27 10:20:05 [ERROR] FTP 上传失败: flows_20251127_101000_001.csv.gz: dial tcp 10.0.0.10:21: i/o timeout
```

## 常见问题

### 1. 配置文件解析失败

确保 `pmacct.conf` 文件中存在 `processor_*` 配置项（key: value），且格式正确。

### 2. FTP 连接失败

检查：
- FTP 服务器地址和端口是否正确
- 网络连接是否正常
- FTP 用户名和密码是否正确
- 防火墙是否允许连接

### 3. 文件上传后未删除

上传成功后程序会自动删除本地文件。如果文件未删除，可能是：
- 上传过程中出现错误（检查日志）
- 文件权限问题

### 4. 数据目录权限问题

确保程序对数据目录有读写权限。Docker 中可能需要调整挂载目录的权限。

## 开发说明

### 模块说明

- **config**：解析 `pmacct.conf` 中的 `processor_*` 配置项
- **writer**：从 stdin 读取数据，滚动写入 gzip 文件
- **uploader**：定时扫描目录，FTP 上传已完成的文件

### 依赖库

- `github.com/jlaffaye/ftp`：FTP 客户端库

## 许可证

MIT
