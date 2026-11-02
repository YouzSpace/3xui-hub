#!/usr/bin/env bash
# ============================================================
# ControlHub xray 节点安装脚本（M0：全程只连面板，GitHub 零依赖）
# 用法（root 直登 / 普通用户都行，脚本自己按身份提权：root 直跑 → sudo → su）：
#   curl -fsSL {HUB}/node-install.sh -o /tmp/controlhub-node-install.sh \
#     && bash /tmp/controlhub-node-install.sh \
#          --hub https://hub.example.com --node my-node --secret xxxx
#       可选：--hub-v6 https://v6.hub.example.com（面板有 IPv6 地址时的兜底通道）
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
# HUB_V6 是可选的面板 IPv6 地址（issue #88）：配了才写进 state.json 的 hub_url_v6，
# agent 以 hub_url 优先、连不上再换它；不传 = state.json 里不出现这个键，行为与改动前一致。
HUB_V6=""
NODE_NAME=""
SECRET=""
while [ $# -gt 0 ]; do
  case "$1" in
    --hub)    HUB="$2"; shift 2 ;;
    --hub-v6) HUB_V6="$2"; shift 2 ;;
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
HUB_V6="${HUB_V6%/}"

# ── 提权：root 直跑 → sudo → su，每一档都有下一步可走 ──────
# 不在面板命令里写死 sudo：Debian 基础镜像常是「root 直登 + 根本没装 sudo」
# （sudo 是 optional 包，连 /etc/sudoers 都不存在），写死 sudo 会死在
# `bash: sudo: command not found` —— 那是 shell 解析阶段的错，脚本一行都跑不到。
# 本脚本以 bash 运行（用到 declare -A），故可用 BASH_SOURCE 自我重跑。
if [ "$(id -u)" != "0" ]; then
  SELF="${BASH_SOURCE[0]:-}"
  if [ -z "$SELF" ] || [ ! -f "$SELF" ]; then
    # 被管道喂给 bash（curl … | bash -s --）时取不到文件路径，无法自我提权
    echo "无法自我提权：脚本是被管道喂给 bash 的（取不到文件路径）。请改用落盘方式重跑："
    echo "  curl -fsSL ${HUB}/node-install.sh -o /tmp/controlhub-node-install.sh \\"
    echo "    && bash /tmp/controlhub-node-install.sh --hub ${HUB} --node ${NODE_NAME} --secret <把面板上那条命令里的 secret 粘到这里>"
    exit 1
  fi
  if command -v sudo >/dev/null 2>&1; then
    echo "==> 当前非 root，用 sudo 重新执行"
    exec sudo bash "$SELF" "$@"
  elif command -v su >/dev/null 2>&1; then
    echo "==> 未检测到 sudo，改用 su 提权（需要输入 root 密码）"
    exec su -c "$(printf '%q ' bash "$SELF" "$@")"
  else
    echo "==> 当前非 root，且 sudo / su 都不可用。请二选一："
    echo "    1) 改用 root 登录后重跑同一条命令；"
    echo "    2) 先以 root 执行：apt-get install -y sudo（Debian/Ubuntu）或 yum install -y sudo（RHEL 系）"
    exit 1
  fi
fi

# ── 依赖检查 ──────────────────────────────────────────────
# python3 必须在此检查：脚本有 4 处用它解析面板清单（下方 manifest 解析），
# 缺失时会在解析处以 127 直接中断，报错与真实原因无关（难排查），故提前拦。
# 缺依赖直接用 apt 自动安装（节点环境即 Debian/Ubuntu，apt 标配）。
declare -A DEP_PKG=([curl]=curl [unzip]=unzip [sha256sum]=coreutils [python3]=python3)
MISSING=()
for dep in curl unzip sha256sum python3; do
  command -v "$dep" >/dev/null 2>&1 || MISSING+=("${DEP_PKG[$dep]}")
done
if [ ${#MISSING[@]} -gt 0 ]; then
  echo "==> 缺少依赖: ${MISSING[*]}"
  if command -v apt-get >/dev/null 2>&1; then
    echo "==> 自动安装缺失依赖"
    # set -e 下裸跑这两条，一旦失败（源不可达 / 无网）脚本会静默退出，
    # 只剩上面半句话，用户看不出是网络还是脚本的问题 —— 这里显式兜错并给出可复制的命令。
    DEBIAN_FRONTEND=noninteractive apt-get update -qq \
      || { echo "apt-get update 失败：请检查网络或软件源（/etc/apt/sources.list）后重跑"; exit 1; }
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${MISSING[@]}" \
      || { echo "依赖安装失败，请手动执行：apt-get install -y ${MISSING[*]}"; exit 1; }
  else
    echo "本机没有 apt-get，请手动安装后重跑，例如："
    echo "  Debian/Ubuntu: apt-get update && apt-get install -y ${MISSING[*]}"
    echo "  RHEL/CentOS:   yum install -y ${MISSING[*]}"
    exit 1
  fi
fi
# 复核：装了还缺 = 真失败（apt 源缺包等），报到具体是哪个
for dep in curl unzip sha256sum python3; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "缺依赖: $dep（自动安装未生效，请手动安装后重跑）"
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
STAMP="$NODE_DIR/.xray.sha256"
# 幂等：已装同一份资产（落地章 SHA 与清单一致）则跳过重下 + 解压，避免重复下载
if [ -x "$NODE_DIR/xray" ] && [ -f "$STAMP" ] && [ "$(cat "$STAMP")" = "$SHA" ]; then
  echo "  已装同版本资产（sha256 匹配），跳过下载"
else
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
  printf '%s' "$SHA" > "$STAMP"
fi
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
AGENT_STAMP="$NODE_DIR/.agent.sha256"
# 幂等：已装同一份 agent（落地章与清单一致）则跳过重下
if [ -x "$NODE_DIR/agent" ] && [ -f "$AGENT_STAMP" ] && [ "$(cat "$AGENT_STAMP")" = "$AGENT_SHA" ]; then
  echo "  已装同版本 agent（sha256 匹配），跳过下载"
else
  # 注意：agent 可能**正在运行**（重复安装/升级场景）——不能直接 -o 覆盖运行中的
  # 可执行文件（内核拒绝写入，curl 报 (23) Failure writing output to destination，
  # 老机器重跑安装脚本必踩）。先下载到临时文件、校验通过后 mv 原子替换
  # （rename 不受"运行中"限制），与 state.json 的原子写同思路。
  AGENT_TMP="$NODE_DIR/agent.new"
  curl -fsSL "$HUB/node-bin/agent/$ARCH" -o "$AGENT_TMP"
  AGENT_ACTUAL=$(sha256sum "$AGENT_TMP" | awk '{print $1}')
  if [ "$AGENT_ACTUAL" != "$AGENT_SHA" ]; then
    echo "agent sha256 校验失败！期望 $AGENT_SHA，实际 $AGENT_ACTUAL（终止）"
    rm -f "$AGENT_TMP"; exit 1
  fi
  chmod +x "$AGENT_TMP"
  mv -f "$AGENT_TMP" "$NODE_DIR/agent"
  printf '%s' "$AGENT_SHA" > "$AGENT_STAMP"
  echo "  agent 就位（sha256 校验通过）"
fi

echo "==> 写入节点状态 state.json（权限 600，含密钥）"
# 用 python3 json.dump 生成：值经 JSON 转义，密钥/名称含特殊字符（" \ 换行）也不会拼坏；
# 先写临时文件再 mv 原子替换，避免写入中途中断留下半截非法 JSON 把 agent 卡死。
STATE_TMP="$NODE_DIR/state.json.tmp"
HUB="$HUB" HUB_V6="$HUB_V6" NODE_NAME="$NODE_NAME" SECRET="$SECRET" \
XRAY_VER="$XRAY_VER" ARCH="$ARCH" STATE_TMP="$STATE_TMP" python3 - <<'PY' || { rm -f "$STATE_TMP"; echo "state.json 生成失败"; exit 1; }
import json, os
state = {
    "hub_url": os.environ["HUB"],
    "node_name": os.environ["NODE_NAME"],
    "node_secret": os.environ["SECRET"],
    "xray_version": os.environ["XRAY_VER"],
    "kernel_arch": os.environ["ARCH"],
}
# issue #88：面板 IPv6 兜底地址（可选）。留空就不写这个键，老节点 state.json 逐字节不变。
if os.environ.get("HUB_V6"):
    state["hub_url_v6"] = os.environ["HUB_V6"]
with open(os.environ["STATE_TMP"], "w") as f:
    json.dump(state, f, indent=2, ensure_ascii=False)
    f.write("\n")
PY
chmod 600 "$STATE_TMP"
mv -f "$STATE_TMP" "$NODE_DIR/state.json"
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
