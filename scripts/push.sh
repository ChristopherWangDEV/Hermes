#!/usr/bin/env bash
# push.sh —— 校验通过才推送。
#
# 为什么需要它：这个仓库长期只有**一个** commit（每次改动 amend 后 force-push），
# 所以"先推后修"会直接改写历史，而不是留下一个可以 revert 的坏提交。
# 本次开发中出现过两次"check 红了还推"——都是命令里 `;` 与 `&&` 的区别
# 被忽略。把顺序写进脚本，就不依赖记性。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MSG="${1:-Hermes 1.2.1}"

echo "==> check（不通过就停在这里，不 commit、不 push）"
bash scripts/check.sh

echo "==> commit"
git add -A
if git diff --cached --quiet; then
  echo "（没有改动，只推标签）"
else
  git commit --amend -m "$MSG"
fi

echo "==> push main + tags"
git push --force origin main
git tag -f v1.2.1 HEAD >/dev/null
git tag -f packages/go/v1.2.1 HEAD >/dev/null
git push --force origin refs/tags/v1.2.1
git push --force origin refs/tags/packages/go/v1.2.1
echo "==> done"
