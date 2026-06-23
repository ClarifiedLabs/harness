#!/usr/bin/env bash
set -euo pipefail

: "${TAG:?TAG is required}"
: "${SOURCE_SHA256:?SOURCE_SHA256 is required}"

tap_dir="${TAP_DIR:?TAP_DIR is required}"
formula_dir="${tap_dir}/Formula"
version="${TAG#v}"
source_url="${SOURCE_URL:-https://github.com/ClarifiedLabs/harness/archive/refs/tags/${TAG}.tar.gz}"

mkdir -p "$formula_dir"

write_binary_formula() {
  local formula_name="$1"
  local class_name="$2"
  local description="$3"
  local binary_name="$4"
  local command_path="$5"

  cat >"${formula_dir}/${formula_name}.rb" <<FORMULA
class ${class_name} < Formula
  desc "${description}"
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
    system "go", "build", "-trimpath", "-ldflags", ldflags.join(" "), "-o", bin/"${binary_name}", "${command_path}"
  end

  test do
    assert_match "${binary_name} v#{version}", shell_output("#{bin}/${binary_name} --version")
  end
end
FORMULA
}

write_binary_formula "harness" "Harness" "Tool-using LLM harness CLI" "harness" "./cmd/harness"
write_binary_formula "harness-model-proxy" "HarnessModelProxy" "Provider and model proxy for harness" "harness-model-proxy" "./cmd/harness-model-proxy"
write_binary_formula "harness-mcp-proxy" "HarnessMcpProxy" "MCP proxy daemon and debug client for harness" "harness-mcp-proxy" "./cmd/harness-mcp-proxy"

cat >"${formula_dir}/harness-full.rb" <<FORMULA
class HarnessFull < Formula
  desc "Meta formula for the harness CLI and proxy binaries"
  homepage "https://github.com/ClarifiedLabs/harness"
  url "${source_url}"
  sha256 "${SOURCE_SHA256}"
  version "${version}"
  license "MIT"

  depends_on "harness"
  depends_on "harness-model-proxy"
  depends_on "harness-mcp-proxy"

  def install
    pkgshare.mkpath
    (pkgshare/"README").write "harness-full installs the harness CLI, model proxy, and MCP proxy formulae.\\n"
  end

  test do
    assert_predicate HOMEBREW_PREFIX/"bin/harness", :exist?
    assert_predicate HOMEBREW_PREFIX/"bin/harness-model-proxy", :exist?
    assert_predicate HOMEBREW_PREFIX/"bin/harness-mcp-proxy", :exist?
  end
end
FORMULA
