#!/usr/bin/env bash
# Copies the OpenAPI descriptions and the Gateway's route list into testdata/,
# each with a first line naming where it came from.
# Usage: scripts/refresh-testdata.sh <workspace root holding the sibling repositories>
set -euo pipefail

root=${1:?usage: refresh-testdata.sh <workspace root>}
here=$(cd "$(dirname "$0")/.." && pwd)
specs="$here/testdata/specs"
mkdir -p "$specs"

# fromgit <repo> <path in repo> <destination>: reads the remote main, never the working tree.
fromgit() {
  local sha
  sha=$(git -C "$root/$1" rev-parse inkly/main)
  { echo "# from $1 $sha"; git -C "$root/$1" show "inkly/main:$2"; } >"$3"
}

fromgit CasaOS api/casaos/openapi.yaml "$specs/casaos.yaml"
fromgit CasaOS-AppManagement api/app_management/openapi.yaml "$specs/app_management.yaml"
fromgit CasaOS-AppManagement api/app_management/openapi_v1.yaml "$specs/app_management_v1.yaml"
fromgit CasaOS-LocalStorage api/local_storage/openapi.yaml "$specs/local_storage.yaml"
fromgit CasaOS-UserService api/user-service/openapi.yaml "$specs/users.yaml"
fromgit ReCasaOS-VulnScan api/vuln_scan/openapi.yaml "$specs/vuln_scan.yaml"

# The VM module is not cloned next to the others: read it from GitHub.
vsha=$(gh api repos/ReCasaOS/CasaOS-VirtManagement/commits/main --jq .sha)
{ echo "# from CasaOS-VirtManagement $vsha"
  gh api repos/ReCasaOS/CasaOS-VirtManagement/contents/api/virt_management/openapi.yaml -H "Accept: application/vnd.github.raw"
} >"$specs/virt_management.yaml"

# The route list comes from a branch of the Gateway, from its working tree.
gsha=$(git -C "$root/_mcp-gateway" rev-parse HEAD)
{ echo "# from _mcp-gateway $gsha"; cat "$root/_mcp-gateway/route/testdata/routes.txt"; } >"$here/testdata/routes.txt"
