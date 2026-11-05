#!/usr/bin/env bash
# 把 editor/vscode 打成 .vsix。
#
# 为什么不用 `vsce`：它是 npm 包（要 npm install），而我们希望打包过程
# **零依赖** —— .vsix 就是一个有固定结构的 zip：`extension/` 目录 +
# `[Content_Types].xml` + `extension.vsixmanifest`。
# 自己拼比引一个工具链更容易审计，也不会把 npm 拉进构建。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/editor/vscode"
OUT_DIR="$ROOT/build/release"
NAME="hermes-vscode"
VERSION="$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$SRC/package.json" | head -1)"
VSIX="$OUT_DIR/${NAME}-${VERSION}.vsix"

[ -n "$VERSION" ] || { echo "读不出版本号" >&2; exit 4; }
mkdir -p "$OUT_DIR"

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

mkdir -p "$STAGE/extension"
cp -R "$SRC/package.json" "$SRC/language-configuration.json" "$STAGE/extension/"
cp -R "$SRC/src" "$SRC/syntaxes" "$STAGE/extension/"
cp "$SRC/README.md" "$SRC/CHANGELOG.md" "$STAGE/extension/"

cat > "$STAGE/[Content_Types].xml" <<'XML'
<?xml version="1.0" encoding="utf-8"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension=".json" ContentType="application/json"/>
  <Default Extension=".js" ContentType="application/javascript"/>
  <Default Extension=".md" ContentType="text/markdown"/>
  <Default Extension=".xml" ContentType="text/xml"/>
  <Default Extension=".vsixmanifest" ContentType="text/xml"/>
</Types>
XML

cat > "$STAGE/extension.vsixmanifest" <<XML
<?xml version="1.0" encoding="utf-8"?>
<PackageManifest Version="2.0.0"
  xmlns="http://schemas.microsoft.com/developer/vsx-schema/2011"
  xmlns:d="http://schemas.microsoft.com/developer/vsx-schema-design/2011">
  <Metadata>
    <Identity Language="en-US" Id="${NAME}" Version="${VERSION}" Publisher="hermes" />
    <DisplayName>Hermes</DisplayName>
    <Description xml:space="preserve">HermesScript language support: syntax highlighting, diagnostics, completion, hover, outline (served by hermes lsp).</Description>
    <Tags>hermes,hermesscript,workflow</Tags>
    <Categories>Programming Languages</Categories>
    <GalleryFlags>Public</GalleryFlags>
    <Properties>
      <Property Id="Microsoft.VisualStudio.Code.Engine" Value="^1.75.0" />
      <Property Id="Microsoft.VisualStudio.Code.ExtensionDependencies" Value="" />
      <Property Id="Microsoft.VisualStudio.Code.ExtensionPack" Value="" />
    </Properties>
  </Metadata>
  <Installation>
    <InstallationTarget Id="Microsoft.VisualStudio.Code"/>
  </Installation>
  <Dependencies/>
  <Assets>
    <Asset Type="Microsoft.VisualStudio.Code.Manifest" Path="extension/package.json" Addressable="true" />
    <Asset Type="Microsoft.VisualStudio.Services.Content.Details" Path="extension/README.md" Addressable="true" />
  </Assets>
</PackageManifest>
XML

rm -f "$VSIX"
( cd "$STAGE" && zip -qr "$VSIX" . )
echo "==> $VSIX"
echo "==> 内容："
unzip -l "$VSIX" | sed 's/^/      /' | tail -n +4
echo
echo "安装："
echo "  code --install-extension $VSIX"
