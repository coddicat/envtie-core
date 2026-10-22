$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot
if (-not $env:GOCACHE) { $env:GOCACHE = Join-Path $repoRoot '.gocache' }
if (-not $env:GOMODCACHE) { $env:GOMODCACHE = Join-Path $repoRoot '.gomodcache' }
$goExe = (Get-Command go -ErrorAction SilentlyContinue).Source
if (-not $goExe) { $goExe = Join-Path $repoRoot '.toolchain/go/bin/go.exe' }
if (-not (Test-Path -LiteralPath $goExe)) { throw 'Go 1.27.2 is required.' }
New-Item -ItemType Directory -Force -Path 'dist' | Out-Null
foreach ($name in @('envtie','adapter-neon','adapter-upstash','adapter-github','adapter-stripe','adapter-sentry')) {
  # -buildvcs=false and an empty build id keep the SHA-256 pin stable across rebuilds.
  & $goExe build -trimpath -buildvcs=false -ldflags '-buildid=' -o (Join-Path 'dist' ($name + '.exe')) ('./cmd/' + $name)
  if ($LASTEXITCODE -ne 0) { throw "Build failed: $name" }
  $hash = (Get-FileHash -Algorithm SHA256 (Join-Path 'dist' ($name + '.exe'))).Hash.ToLowerInvariant()
  Write-Output "$name.exe  $hash"
}
$arch = (& $goExe env GOARCH).Trim()
$pins = [ordered]@{}
foreach ($name in @('adapter-neon','adapter-upstash','adapter-github','adapter-stripe','adapter-sentry')) {
  $adapter = ($name -replace '^adapter-','') + '@1'
  $artifact = Get-Item -LiteralPath (Join-Path 'dist' ($name + '.exe'))
  $hash = (Get-FileHash -Algorithm SHA256 $artifact.FullName).Hash.ToLowerInvariant()
  $urlAdapter = [Uri]::EscapeDataString($adapter)
  $pins[$adapter] = [ordered]@{ "windows/$arch" = [ordered]@{
    sha256 = $hash
    size = $artifact.Length
    url = "/v1/adapter-artifacts/$urlAdapter/windows/$arch"
  } }
}
$catalogPath = Join-Path 'dist' 'adapters.json'
$json = @{ release = 'local'; adapters = $pins } | ConvertTo-Json -Depth 6
$utf8 = New-Object System.Text.UTF8Encoding $false
[System.IO.File]::WriteAllText((Join-Path $repoRoot $catalogPath), $json.Trim() + "`n", $utf8)
Write-Output "wrote $catalogPath"
