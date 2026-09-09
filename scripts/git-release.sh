#!/usr/bin/env bash
# ==============================================================================
# 分布式存储日志分析系统 - 一键推送 Tag 触发 GitHub 自动构建发布 Release
# ==============================================================================

set -e

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$PROJECT_ROOT"

echo "=================================================================="
echo "    🚀 推送 Git Tag 触发 GitHub 自动编译发布 Release              "
echo "=================================================================="

# 1. 检查 git 状态
if [ -n "$(git status --porcelain)" ]; then
    echo "[WARN] 检测到工作区有未提交的变更！"
    git status -s
    read -r -p "是否先自动提交这些变更并推送到 main 分支？[Y/n]: " COMMIT_CHOICE
    if [ "$COMMIT_CHOICE" != "n" ] && [ "$COMMIT_CHOICE" != "N" ]; then
        read -r -p "请输入提交信息 (默认: chore: update and prepare release): " MSG
        if [ -z "$MSG" ]; then MSG="chore: update and prepare release"; fi
        git add .
        git commit -m "$MSG"
        echo "[INFO] 正在推送到远程 main 分支..."
        git push origin main
    fi
fi

# 2. 列出已有最近的 tag
echo ""
echo "▶ 最近已有的 Tags:"
git tag --sort=-v:refname | head -n 5 || echo "(暂无 tags)"

echo ""
read -r -p "请输入要发布的版本 Tag (例如 v1.0.0): " RELEASE_TAG
if [ -z "$RELEASE_TAG" ]; then
    echo "[ERROR] Tag 不能为空！"
    exit 1
fi

# 格式校验建议 (以 v 开头)
if [[ "$RELEASE_TAG" != v* ]]; then
    RELEASE_TAG="v$RELEASE_TAG"
fi

# 检查 tag 是否已存在
if git rev-parse "$RELEASE_TAG" >/dev/null 2>&1; then
    echo "[WARN] Tag $RELEASE_TAG 在本地已存在！"
    read -r -p "是否删除旧 Tag 并重新创建？[y/N]: " OVERWRITE
    if [ "$OVERWRITE" == "y" ] || [ "$OVERWRITE" == "Y" ]; then
        git tag -d "$RELEASE_TAG"
        git push origin --delete "$RELEASE_TAG" 2>/dev/null || true
    else
        echo "已取消操作。"
        exit 0
    fi
fi

# 3. 创建 Tag
echo "[1/2] 创建本地 Git 标签: $RELEASE_TAG..."
git tag -a "$RELEASE_TAG" -m "Release $RELEASE_TAG: 分布式存储日志分析系统"

# 4. 推送 Tag
echo "[2/2] 推送 Tag 到远程 GitHub 仓库 (git push origin $RELEASE_TAG)..."
git push origin "$RELEASE_TAG"

echo ""
echo "=================================================================="
echo "    🎉 Tag 推送成功！GitHub Actions 自动化流水线已触发！          "
echo "=================================================================="
echo "  ▶ 触发标签: $RELEASE_TAG"
echo "  ▶ 您可前往 GitHub 仓库 Actions 页面查看实时构建流水线:"
echo "     https://github.com/JayYang1991/distribute-storage-log-alalyzer/actions"
echo "  ▶ 构建完成后，发布包将自动挂载至 Releases 页面:"
echo "     https://github.com/JayYang1991/distribute-storage-log-alalyzer/releases"
echo "=================================================================="
