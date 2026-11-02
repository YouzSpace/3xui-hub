#!/usr/bin/env bash
# ============================================================
# ControlHub agent 交叉编译（仅 linux-64）→ 面板资产目录
# 用法：VERSION=1.0.0 ./build.sh
# 产物直进 backend/storage/app/xray/node-bin/agent/（随面板分发）
# ============================================================
set -euo pipefail
cd "$(dirname "$0")"

VER="${VERSION:-1.0.0}"
OUT="${OUT:-../backend/storage/app/xray/node-bin/agent}"
mkdir -p "$OUT"

build() { # <goarch> <goarm> <目标文件名>
  local goarch="$1" goarm="$2" name="$3"
  echo "==> $name"
  GOOS=linux GOARCH="$goarch" GOARM="$goarm" CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -buildid= -X main.agentVersion=$VER" -o "$OUT/$name" .
}

build amd64 "" controlhub-agent-linux-64

cd "$OUT"
sha256sum controlhub-agent-linux-* > SHA256SUMS

# 更新面板 manifest.json 的 agent 段（sha256/size 与产物同步，安装脚本据此校验）
ABS_OUT="$(pwd)"
MANIFEST="$(cd "$ABS_OUT/../.." && pwd)/manifest.json"
# Windows 开发机：python 是原生程序，不认 MSYS 的 /f/... 路径，cygpath -m 转成 F:/... 正斜杠
if command -v cygpath >/dev/null 2>&1; then
  ABS_OUT="$(cygpath -m "$ABS_OUT")"
  MANIFEST="$(cygpath -m "$MANIFEST")"
fi
PYBIN=""
# 优先 python3（Linux 标准），但必须是「真能跑」的（Windows 上 python3 可能是 Store 存根，执行即失败）
if command -v python3 >/dev/null 2>&1 && python3 -c 'import json,sys' >/dev/null 2>&1; then
  PYBIN=python3
elif command -v python >/dev/null 2>&1 && python -c 'import json,sys' >/dev/null 2>&1; then
  PYBIN=python
fi
if [ -z "$PYBIN" ]; then
  echo "警告：未找到可用 python，跳过 manifest agent 段更新（请手工同步 sha256）"
  exit 0
fi
"$PYBIN" - "$ABS_OUT" "$MANIFEST" "$VER" "${UPLOAD_TAG:-}" <<'PY'
import hashlib, json, os, sys
out, manifest_path, ver = sys.argv[1], sys.argv[2], sys.argv[3]
upload_tag = (sys.argv[4].strip() if len(sys.argv) > 4 else "")
archs = ["linux-64"]
assets = []
for arch in archs:
    fname = "controlhub-agent-%s" % arch
    p = os.path.join(out, fname)
    with open(p, "rb") as f:
        data = f.read()
    asset = {"arch": arch, "file": fname,
             "sha256": hashlib.sha256(data).hexdigest(),
             "size": os.path.getsize(p)}
    # 二进制只放 GitHub Release 附件（不进 git）：面板本地缺文件时可从 url 兜底下载。
    # 没给 UPLOAD_TAG 就不写 url（保持原 manifest 结构，本地/离线部署照旧）。
    if upload_tag:
        asset["url"] = "https://github.com/YouzSpace/3xui-hub/releases/download/%s/%s" % (upload_tag, fname)
    assets.append(asset)
with open(manifest_path, encoding="utf-8") as f:
    manifest = json.load(f)
manifest["agent"] = {"version": ver, "assets": assets}
with open(manifest_path, "w", encoding="utf-8") as f:
    json.dump(manifest, f, indent=2, ensure_ascii=False)
    f.write("\n")
print("manifest agent 段已更新（%s%s）" % (ver, "，url=%s" % upload_tag if upload_tag else "，无 url"))
PY

# 可选：把产物上传到 GitHub Release 附件（issue #116 3.1 止损）。
# 只在显式给了 UPLOAD_TAG 时才上传；平时编译 + manifest 流程不受影响。
# 用法：UPLOAD_TAG=v2.9.1 ./build.sh
if [ -n "${UPLOAD_TAG:-}" ]; then
  if command -v gh >/dev/null 2>&1 && [ -n "${GH_TOKEN:-}" ]; then
    echo "==> 上传 agent 二进制到 Release ${UPLOAD_TAG}"
    gh release upload "$UPLOAD_TAG" "$OUT"/controlhub-agent-linux-* --clobber || true
  else
    echo "警告：已设 UPLOAD_TAG 但缺 gh 或 GH_TOKEN，跳过 Release 上传"
  fi
fi

echo "==> 完成：$OUT"
ls -la
