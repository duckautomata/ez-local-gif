# Runs the Go toolchain inside the golang:1.26-trixie image with the repo
# mounted at /src (Go is not installed on the Windows host). Module and
# build caches persist in named Docker volumes so repeated runs are fast.
#
#   .\scripts\go.ps1 vet ./...
#   .\scripts\go.ps1 test ./...
#   .\scripts\go.ps1 build -o bin/ezlg ./cmd/ezlg
#
# Env passthrough: any EZLG_* variables set in the calling shell.
$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$envArgs = @()
Get-ChildItem Env: | Where-Object { $_.Name -like 'EZLG_*' } | ForEach-Object { $envArgs += @('-e', "$($_.Name)=$($_.Value)") }
docker run --rm `
  -v "${repo}:/src" -w /src `
  -v ezlg-gomod:/go/pkg/mod -v ezlg-gocache:/root/.cache/go-build `
  -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 `
  @envArgs `
  golang:1.26-trixie go @args
exit $LASTEXITCODE
