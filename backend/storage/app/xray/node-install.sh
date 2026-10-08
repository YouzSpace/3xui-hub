#!/usr/bin/env bash
# ============================================================
# ControlHub xray 节点安装脚本（M0：全程只连面板，GitHub 零依赖）
# 用法：
#   curl -fsSL {HUB}/node-install.sh | sudo bash -s -- \
#       --hub https://hub.example.com --node my-node --secret xxxx
# 动作：
#   1. 检测 CPU 架构 → 映射面板资产 arch
#   2. 从面板取 default 版本清单 → 下载对应 xray zip + sha256 自校验
#   3. 落地 /opt/controlhub-node/{xray,agent,state.json}
#   4. 装 systemd 守护服务 controlhub-node.service
#   5. 首次 register + 起 xray（agent 接管后续）
# ============================================================
set -euo pipefail

NODE_DIR="/opt/controlhub-node"
SERVICE="controlhub-node.service"

# ── 解析参数 ──────────────────────────────────────────────
HUB=""
NODE_NAME=""
SECRET=""
while [ $# -gt 0 ]; do
  case "$1" in
    --hub)    HUB="$2"; shift 2 ;;
    --node)   NODE_NAME="$2"; shift 2 ;;
    --secret) SECRET="$2"; shift 2 ;;
    *) echo "未知参数: $1"; exit 1 ;;
  esac
done

if [ -z "$HUB" ] || [ -z "$NODE_NAME" ] || [ -z "$SECRET" ]; then
  echo "缺少必需参数（--hub/--node/--secret）"
  exit 1
fi
HUB="${HUB%/}"   # 只去掉结尾斜杠，不动协议里的 //

# ── 需要 root ────────────────────────────────────────────
if [ "$(id -u)" != "0" ]; then
  echo "请用 sudo 运行（需要 root 写 /opt、装 systemd）"
  exit 1
fi

# ── 依赖检查 ──────────────────────────────────────────────
for dep in curl unzip sha256sum; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "缺依赖: $dep（Debian/Ubuntu: apt-get install -y $dep）"
    exit 1
  fi
done

echo "==> 架构检测"
MACHINE=$(uname -m)
case "$MACHINE" in
  x86_64|amd64)  ARCH="linux-64" ;;
  *) echo "不支持的 CPU 架构: $MACHINE（当前仅支持 Debian12 x86_64）"; exit 1 ;;
esac
echo "  架构: $MACHINE → 资产 arch: $ARCH"

echo "==> 拉取面板资产清单"
MANIFEST_JSON=$(curl -fsSL "$HUB/node-bin/manifest.json")
# python3 取 default 版本 + 该架构条目（Debian 12 自带）
DEFAULT_VER=$(printf '%s' "$MANIFEST_JSON" | python3 -c 'import sys,json;print(json.load(sys.stdin)["default"])')
FILE=$(printf '%s' "$MANIFEST_JSON" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);v=d["default"];print([a["file"] for a in d["versions"][v]["assets"] if a["arch"]=="'$ARCH'"][0])')
SHA=$(printf '%s' "$MANIFEST_JSON" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);v=d["default"];print([a["sha256"] for a in d["versions"][v]["assets"] if a["arch"]=="'$ARCH'"][0])')
echo "  默认内核版本: v$DEFAULT_VER，资产: $FILE"

echo "==> 下载 xray v$DEFAULT_VER ($ARCH)"
mkdir -p "$NODE_DIR"
ZIP="$NODE_DIR/xray.zip"
curl -fsSL "$HUB/node-bin/$DEFAULT_VER/$FILE" -o "$ZIP"
# sha256 自校验（面板 manifest 下发 + 下载后本地复验，双保险）
ACTUAL=$(sha256sum "$ZIP" | awk '{print $1}')
if [ "$ACTUAL" != "$SHA" ]; then
  echo "sha256 校验失败！期望 $SHA，实际 $ACTUAL（网络可能被篡改，终止）"
  rm -f "$ZIP"; exit 1
fi
echo "  sha256 校验通过: $ACTUAL"

echo "==> 解压内核"
unzip -oq "$ZIP" -d "$NODE_DIR"
chmod +x "$NODE_DIR/xray"
rm -f "$ZIP"
# 先收全量输出再截取：head -1 会让 xray 收 SIGPIPE，pipefail 下整条管道返回 141，
# 被 set -e 当成失败。原写法靠 `|| echo unknown` 压住不中断，副作用是 unknown 被拼进
# 版本号成了两行，写进 state.json 就是非法 JSON（agent 起不来）。这里改成不产生 SIGPIPE。
VER_RAW=$("$NODE_DIR/xray" version 2>/dev/null) || VER_RAW=""
XRAY_VER=$(printf '%s\n' "$VER_RAW" | head -1 | awk '{print $2}')
XRAY_VER=${XRAY_VER:-unknown}
echo "  xray 就位，版本: $XRAY_VER"

echo "==> 下载 agent 二进制（Go 版）"
AGENT_SHA=$(printf '%s' "$MANIFEST_JSON" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print([a["sha256"] for a in d["agent"]["assets"] if a["arch"]=="'$ARCH'"][0])')
curl -fsSL "$HUB/node-bin/agent/$ARCH" -o "$NODE_DIR/agent"
AGENT_ACTUAL=$(sha256sum "$NODE_DIR/agent" | awk '{print $1}')
if [ "$AGENT_ACTUAL" != "$AGENT_SHA" ]; then
  echo "agent sha256 校验失败！期望 $AGENT_SHA，实际 $AGENT_ACTUAL（终止）"
  rm -f "$NODE_DIR/agent"; exit 1
fi
chmod +x "$NODE_DIR/agent"
echo "  agent 就位（sha256 校验通过）"

echo "==> 写入节点状态 state.json（权限 600，含密钥）"
cat > "$NODE_DIR/state.json" <<STATE
{
  "hub_url": "$HUB",
  "node_name": "$NODE_NAME",
  "node_secret": "$SECRET",
  "xray_version": "$XRAY_VER",
  "kernel_arch": "$ARCH"
}
STATE
chmod 600 "$NODE_DIR/state.json"

echo "==> 安装 systemd 守护服务"
cat > "/etc/systemd/system/$SERVICE" <<SVC
[Unit]
Description=ControlHub xray node agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$NODE_DIR/agent
Restart=always
RestartSec=5
Environment=NODE_DIR=$NODE_DIR

[Install]
WantedBy=multi-user.target
SVC

systemctl daemon-reload
systemctl enable "$SERVICE"

echo "==> 启动 agent（首装自动 register + 拉 config + 起 xray）"
systemctl restart "$SERVICE"
sleep 3
if systemctl is-active --quiet "$SERVICE"; then
  echo "=========================================="
  echo "节点安装完成：$NODE_NAME（$HUB）"
  echo "服务: $SERVICE（systemctl status 查看）"
  echo "日志: journalctl -u $SERVICE -f"
  echo "状态: 面板 节点管理 页将显示 online"
  echo "=========================================="
else
  echo "agent 启动后未保持运行，请查日志: journalctl -u $SERVICE -n 50"
  exit 1
fi
