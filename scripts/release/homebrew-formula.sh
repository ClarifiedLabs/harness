#!/usr/bin/env bash
set -euo pipefail

: "${TAG:?TAG is required}"
: "${SOURCE_SHA256:?SOURCE_SHA256 is required}"

tap_dir="${TAP_DIR:?TAP_DIR is required}"
formula_dir="${tap_dir}/Formula"
formula_path="${formula_dir}/harness.rb"
version="${TAG#v}"
source_url="${SOURCE_URL:-https://github.com/ClarifiedLabs/harness/archive/refs/tags/${TAG}.tar.gz}"

mkdir -p "$formula_dir"
cat >"$formula_path" <<FORMULA
class Harness < Formula
  desc "Tool-using LLM harness CLI"
  homepage "https://github.com/ClarifiedLabs/harness"
  url "${source_url}"
  sha256 "${SOURCE_SHA256}"
  version "${version}"
  license "MIT"

  depends_on "go" => :build

  def install
    ldflags = %W[
      -s -w
      -X harness/internal/buildinfo.Version=v#{version}
    ]
    system "go", "build", "-trimpath", "-ldflags", ldflags.join(" "), "-o", bin/"harness", "./cmd/harness"
    system "go", "build", "-trimpath", "-ldflags", ldflags.join(" "), "-o", bin/"harness-model-proxy", "./cmd/harness-model-proxy"
    system "go", "build", "-trimpath", "-ldflags", ldflags.join(" "), "-o", bin/"harness-mcp-proxy", "./cmd/harness-mcp-proxy"
  end

  test do
    assert_match "harness v#{version}", shell_output("#{bin}/harness --version")
    assert_match "harness-model-proxy v#{version}", shell_output("#{bin}/harness-model-proxy --version")
    assert_match "harness-mcp-proxy v#{version}", shell_output("#{bin}/harness-mcp-proxy --version")
  end
end
FORMULA
