# ============================================
# ControlHub - 单容器部署（Go 后端 + 内嵌前端）
# 多阶段构建：node 构建前端 → golang 编译（-tags=embed，前端打进二进制）
#            → Debian + Nginx + MySQL + Supervisor 运行
# 运行期不再有独立的前端静态目录：nginx 只做入口反代，页面由 Go 从二进制里吐。
# ============================================

# ── 阶段 1：构建前端（产物落 server/internal/web/dist）──
FROM node:22-slim AS frontend
WORKDIR /build
# 先只拷依赖清单：package-lock 没变时复用 npm ci 这一层，改源码不重装依赖
COPY frontend/package.json frontend/package-lock.json ./frontend/
RUN cd frontend && npm ci --no-audit --no-fund
COPY frontend/ ./frontend/
# vite.config 的 outDir 指向 ../server/internal/web/dist（项目根之外，已开 emptyOutDir）
RUN cd frontend && npm run build

# ── 阶段 2：编译 Go 后端（前端 go:embed 进二进制）──
FROM golang:1.25 AS builder
WORKDIR /build
# 依赖单独一层：go.mod/go.sum 没变时直接复用模块缓存
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download
COPY server/ ./server/
# 前端产物必须在上一步之后落位：go:embed all:dist 在 go build 时读磁盘上的 dist
COPY --from=frontend /build/server/internal/web/dist ./server/internal/web/dist/
RUN cd server && CGO_ENABLED=0 go build -tags=embed -o controlhub-server ./cmd/server

# ── 阶段 3：运行镜像 ──
FROM debian:bookworm-slim

# 系统依赖（Nginx + MySQL + Supervisor + 节点安装脚本依赖的 curl/unzip）
RUN apt-get update && apt-get install -y \
    nginx \
    mysql-server \
    supervisor \
    curl \
    unzip \
    && rm -rf /var/lib/apt/lists/*

# 工作目录：Go 以 /var/www/html/server 为工作目录启动，
# 配置与资产按 ../.env、../assets 解析到 /var/www/html 下。
WORKDIR /var/www/html

# Go 后端二进制（自带前端）+ 建表脚本
COPY --from=builder /build/server/controlhub-server /var/www/html/server/controlhub-server
COPY server/schema/ /var/www/html/server/schema/
# 节点资产（Xray 内核 zip / 安装脚本 / geo 清单）：后端按 ../assets 读取，随面板分发
COPY assets/ /var/www/html/assets/
# 部署配置 + 管理命令
COPY docker/ /var/www/docker/
COPY 3hub /usr/local/bin/3hub

# 配置文件
RUN cp /var/www/docker/nginx.conf /etc/nginx/sites-available/default \
    && cp /var/www/docker/supervisord.conf /etc/supervisor/conf.d/supervisord.conf \
    && chmod +x /usr/local/bin/3hub /var/www/html/server/controlhub-server /var/www/docker/entrypoint.sh

# 数据目录
RUN mkdir -p /var/lib/mysql /var/run/mysqld /var/run/nginx \
    && chown -R mysql:mysql /var/lib/mysql /var/run/mysqld

# 环境变量（Go 后端直连 MySQL；APP_KEY 由 entrypoint 生成）
ENV DB_HOST=127.0.0.1 \
    DB_PORT=3306 \
    DB_DATABASE=controlhub \
    DB_USERNAME=root \
    DB_PASSWORD= \
    APP_KEY= \
    APP_URL=http://localhost

# 端口
EXPOSE 80

# 入口点
ENTRYPOINT ["/bin/bash", "/var/www/docker/entrypoint.sh"]
