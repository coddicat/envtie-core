# Non-mutating Neon and Upstash preflight. Prints presence and HTTP status
# codes only. It does not print credential values, response bodies, or tokens,
# and it does not load the repository .env.
param(
    [string]$Blueprint = '',
    [switch]$Examples
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'lib.ps1')
Set-Location $script:RepoRoot
$script:Failures = @()

function Add-Failure([string]$Message) {
    $script:Failures = @($script:Failures) + $Message
    Write-Host "FAIL $Message"
}

function Add-Ok([string]$Message) { Write-Host "ok   $Message" }
function Add-Skip([string]$Message) { Write-Host "skip $Message" }

function Test-EnvPresent([string]$Name) {
    return -not [string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($Name, 'Process'))
}

function Test-Placeholder([string]$Value) {
    return $Value.Contains('REPLACE_WITH_') -or $Value -match '^your-'
}

function Get-ControlPlaneUrl {
    $raw = [Environment]::GetEnvironmentVariable('ENVTIE_API_URL', 'Process')
    if ([string]::IsNullOrWhiteSpace($raw)) { return 'http://localhost:8080' }
    $raw = $raw.Trim().TrimEnd('/')
    if ($raw -match '^https://[A-Za-z0-9.-]+(?::[0-9]+)?$' -or $raw -match '^http://(localhost|127\.0\.0\.1)(?::[0-9]+)?$') {
        return $raw
    }
    throw 'ENVTIE_API_URL must be an https host or http://localhost with no user info.'
}

function Get-StatusCode([string]$Uri, [hashtable]$Headers) {
    try {
        $params = @{ Uri = $Uri; UseBasicParsing = $true; TimeoutSec = 20; Method = 'Get' }
        if ($null -ne $Headers) { $params.Headers = $Headers }
        $response = Invoke-WebRequest @params
        return [int]$response.StatusCode
    } catch {
        $response = $_.Exception.Response
        if ($null -ne $response -and $null -ne $response.StatusCode) { return [int]$response.StatusCode }
        return 0
    }
}

function Get-Health([string]$Base) {
    $code = 0
    $ok = $false
    try {
        $response = Invoke-WebRequest -Uri ($Base + '/healthz') -UseBasicParsing -TimeoutSec 20
        $code = [int]$response.StatusCode
        if ($code -eq 200 -and [string]$response.Content -match '"status"\s*:\s*"ok"') { $ok = $true }
    } catch {
        $response = $_.Exception.Response
        if ($null -ne $response -and $null -ne $response.StatusCode) { $code = [int]$response.StatusCode }
    }
    return @{ Ok = $ok; Code = $code }
}

function Get-FieldValues([string]$Text, [string]$Field) {
    $values = @()
    foreach ($match in [regex]::Matches($Text, "(?m)^\s*${Field}:\s*(\S+)\s*$")) {
        $values += $match.Groups[1].Value
    }
    return $values
}

function Test-CanonicalBlueprint([string]$RelativePath, [string[]]$BindingLines) {
    $path = Join-Path $script:RepoRoot $RelativePath
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        Add-Failure "$RelativePath is missing"
        return
    }
    $text = [IO.File]::ReadAllText($path)
    if ($text.Contains('://') -or $text -match '(?im)^.*(password|api_key|secret)\s*:') {
        Add-Failure "$RelativePath contains a secret-shaped value"
    }
    $required = @(
        'name: consumer-bench',
        'adapter: neon@1',
        'adapter: upstash@1',
        'project_id: REPLACE_WITH_NEON_PROJECT_ID',
        'database_name: REPLACE_WITH_NEON_DATABASE',
        'role_name: REPLACE_WITH_NEON_ROLE',
        'platform: aws',
        'primary_region: us-east-1',
        'expires_in: 1d',
        'protect: false'
    ) + $BindingLines
    foreach ($line in $required) {
        if (-not $text.Contains($line)) { Add-Failure "$RelativePath is missing $line" }
    }
    if ($text -match '(?m)^\s*(executable|sha256):') {
        Add-Failure "$RelativePath must leave official adapter installation to the runner"
    } else {
        Add-Ok "$RelativePath uses logical official adapters"
    }
}

function Test-BlueprintFile([string]$RelativePath, [bool]$Live) {
    $path = Join-Path $script:RepoRoot $RelativePath
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        Add-Failure "blueprint $RelativePath is missing"
        return
    }
    $text = [IO.File]::ReadAllText($path)
    if ($text.Contains('://') -or $text -match '(?im)^.*(password|api_key|secret)\s*:') {
        Add-Failure 'blueprint contains a secret-shaped value'
    }
    $executables = @(Get-FieldValues $text 'executable')
    $pins = @(Get-FieldValues $text 'sha256')
    if ($executables.Count -ne $pins.Count) {
        Add-Failure 'blueprint executables and sha256 pins do not pair'
        return
    }
    $cli = Join-Path $script:RepoRoot 'dist/envtie.exe'
    $canValidate = Test-Path -LiteralPath $cli -PathType Leaf
    for ($i = 0; $i -lt $executables.Count; $i++) {
        $executable = $executables[$i]
        $pin = $pins[$i]
        if ($executable -match '^[A-Za-z]:' -or $executable.StartsWith('/') -or $executable.StartsWith('\') -or $executable.Contains('..')) {
            Add-Failure "executable must be relative to the runner working directory: $executable"
            continue
        }
        $binary = Join-Path $script:RepoRoot ($executable.Replace('/', '\'))
        if (Test-Placeholder $pin) {
            Add-Failure "pin for $executable is a placeholder. schemas/blueprint.schema.json requires 64 hex characters, so schema validation waits until it is replaced."
            continue
        }
        if ($pin -notmatch '^[a-fA-F0-9]{64}$') {
            Add-Failure "pin for $executable is not 64 hex characters"
            continue
        }
        if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
            Add-Failure "missing binary $executable"
            continue
        }
        $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $binary).Hash.ToLowerInvariant()
        if ($actual -ne $pin.ToLowerInvariant()) {
            Add-Failure "pin does not match $executable"
        } else {
            Add-Ok "pin matches $executable"
        }
    }
    if ($Live -and $canValidate -and -not ($pins | Where-Object { $_ -notmatch '^[a-fA-F0-9]{64}$' })) {
        & $cli blueprint validate $path | Write-Host
        if ($LASTEXITCODE -ne 0) { Add-Failure 'envtie blueprint validate failed' }
        else { Add-Ok 'envtie blueprint validate passed' }
    } elseif ($Live -and -not $canValidate) {
        Add-Skip 'envtie blueprint validate (dist/envtie.exe is absent)'
    }
    return $text
}

function Test-Tools {
    $go = Join-Path $script:RepoRoot '.toolchain/go/bin/go.exe'
    $found = Test-Path -LiteralPath $go
    if (-not $found) { $found = $null -ne (Get-Command go -ErrorAction SilentlyContinue) }
    if ($found) { Add-Ok 'go is available' } else { Add-Failure 'go is not available' }
}

function Test-Credentials([string]$Text, [bool]$Live) {
    $names = @('NEON_API_KEY', 'UPSTASH_EMAIL', 'UPSTASH_API_KEY')
    if ($Text.Contains('github.environment.')) {
        $names += @('GITHUB_TOKEN', 'GITHUB_REPOSITORY', 'ENVTIE_GITHUB_ENVIRONMENT')
    }
    foreach ($name in $names) {
        $present = Test-EnvPresent $name
        Write-Host ("credential {0}: {1}" -f $name, $(if ($present) { 'present' } else { 'missing' }))
        if ($Live -and -not $present) { Add-Failure "missing $name" }
    }
}

function Test-NeonAccess([string]$Text) {
    if (-not (Test-EnvPresent 'NEON_API_KEY')) {
        Add-Skip 'Neon project access (NEON_API_KEY missing)'
        return
    }
    $ids = @(Get-FieldValues $Text 'project_id')
    $checked = $false
    foreach ($id in $ids) {
        if (Test-Placeholder $id) { continue }
        $checked = $true
        if ($id -notmatch '^[A-Za-z0-9][A-Za-z0-9_-]{0,80}$') {
            Add-Failure 'Neon project id is not a safe path segment'
            continue
        }
        $key = [Environment]::GetEnvironmentVariable('NEON_API_KEY', 'Process')
        $code = Get-StatusCode -Uri ("https://console.neon.tech/api/v2/projects/" + $id) -Headers @{ Authorization = "Bearer $key"; Accept = 'application/json' }
        Remove-Variable key
        if ($code -eq 200) { Add-Ok 'Neon project access HTTP 200' }
        else { Add-Failure "Neon project access HTTP $code" }
    }
    if (-not $checked) { Add-Skip 'Neon project access (project id is still a placeholder)' }
}

function Test-UpstashAccess {
    if (-not (Test-EnvPresent 'UPSTASH_EMAIL') -or -not (Test-EnvPresent 'UPSTASH_API_KEY')) {
        Add-Skip 'Upstash management list (UPSTASH_EMAIL or UPSTASH_API_KEY missing)'
        return
    }
    $email = [Environment]::GetEnvironmentVariable('UPSTASH_EMAIL', 'Process')
    $key = [Environment]::GetEnvironmentVariable('UPSTASH_API_KEY', 'Process')
    $basic = [Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes("${email}:${key}"))
    $code = Get-StatusCode -Uri 'https://api.upstash.com/v2/redis/databases?credentials=hide' -Headers @{ Authorization = "Basic $basic"; Accept = 'application/json' }
    Remove-Variable email, key, basic
    if ($code -eq 200) { Add-Ok 'Upstash management list HTTP 200' }
    else { Add-Failure "Upstash management list HTTP $code" }
}

if ($Examples) {
    Test-CanonicalBlueprint -RelativePath 'blueprints/neon-upstash-local.yaml' -BindingLines @(
        'to: dotenv.DATABASE_URL', 'to: dotenv.UPSTASH_REDIS_REST_URL', 'to: dotenv.UPSTASH_REDIS_REST_TOKEN'
    )
    Test-CanonicalBlueprint -RelativePath 'blueprints/neon-upstash-windows.yaml' -BindingLines @(
        'to: dotenv.DATABASE_URL', 'to: dotenv.UPSTASH_REDIS_REST_URL', 'to: dotenv.UPSTASH_REDIS_REST_TOKEN'
    )
    Test-CanonicalBlueprint -RelativePath 'blueprints/neon-upstash-ci.yaml' -BindingLines @(
        'to: github.environment.secret.DATABASE_URL', 'to: github.environment.variable.UPSTASH_REDIS_REST_URL', 'to: github.environment.secret.UPSTASH_REDIS_REST_TOKEN'
    )
    Write-Host 'schema: official adapters are resolved and pinned by the control plane when the blueprint is saved.'
    if ($script:Failures.Count -gt 0) { exit 1 }
    Write-Host 'preflight examples passed'
    exit 0
}

if (-not $Blueprint) { $Blueprint = 'blueprints/neon-upstash-windows.yaml' }
Test-Tools
$text = Test-BlueprintFile $Blueprint $true
if ($text) {
    Test-Credentials $text $true
    try {
        $health = Get-Health (Get-ControlPlaneUrl)
        if ($health.Ok) { Add-Ok 'control plane healthy' }
        else {
            $code = [string]$health.Code
            if ($code -eq '0') { $code = '000' }
            Add-Failure "control plane unhealthy HTTP $code"
        }
    } catch {
        Add-Failure 'control plane URL rejected'
    }
    Test-NeonAccess $text
    if ($text.Contains('adapter: upstash@1')) { Test-UpstashAccess }
}
if ($script:Failures.Count -gt 0) { exit 1 }
Write-Host 'preflight passed'
