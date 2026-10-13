# Runs an EnvTie.dev runner against the local control plane from the repository
# root. Builds the CLI and official adapters, then polls for approved jobs.
# The runner token comes from ENVTIE_RUNNER_TOKEN, .envtie/runner.token,
# or a hidden prompt. Do not pass the token as an argument.
param(
    # Accepted only so a command-line token can be rejected. It is never used.
    [string]$Token,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$Remaining
)
$ErrorActionPreference = 'Stop'
if ($PSBoundParameters.ContainsKey('Token') -or $PSBoundParameters.ContainsKey('Remaining')) {
    [void]$PSBoundParameters.Remove('Token')
    [void]$PSBoundParameters.Remove('Remaining')
    Remove-Variable -Name Token, Remaining -ErrorAction SilentlyContinue
    throw 'Do not pass the runner token on the command line. Set ENVTIE_RUNNER_TOKEN, store it in .envtie/runner.token, or enter it at the prompt.'
}

. (Join-Path $PSScriptRoot 'lib.ps1')
$root = $script:RepoRoot
$script:TokenPath = Join-Path $root '.envtie/runner.token'

function Test-RunnerToken([string]$Value) {
    return $Value -match '^[A-Za-z0-9_-]{16,256}$'
}

function Get-RuleSid($Identity) {
    if ($Identity -is [System.Security.Principal.SecurityIdentifier]) { return $Identity.Value }
    return $Identity.Translate([System.Security.Principal.SecurityIdentifier]).Value
}

function Get-OwnerOnlyGrant([string]$Path) {
    $item = Get-Item -LiteralPath $Path -Force
    # A directory grant has to be inheritable. Full control on the directory object
    # alone leaves new files with an empty ACL, which denies even the owner.
    if ($item.PSIsContainer) { return '(OI)(CI)(F)' }
    return '(F)'
}

function Set-OwnerOnlyFileAcl([string]$Path) {
    # icacls works on Windows PowerShell 5.1, where Set-Acl can require SeSecurityPrivilege.
    $account = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
    $grant = Get-OwnerOnlyGrant $Path
    & icacls.exe $Path /inheritance:r | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not restrict permissions on .envtie/runner.token.' }
    & icacls.exe $Path /grant:r "${account}:${grant}" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not restrict permissions on .envtie/runner.token.' }
    $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    foreach ($rule in @((Get-Acl -LiteralPath $Path).Access)) {
        try { $ruleSid = Get-RuleSid $rule.IdentityReference } catch { throw 'Could not restrict permissions on .envtie/runner.token.' }
        if ($ruleSid -ne $sid) {
            & icacls.exe $Path /remove "$($rule.IdentityReference.Value)" | Out-Null
            if ($LASTEXITCODE -ne 0) { throw 'Could not restrict permissions on .envtie/runner.token.' }
        }
    }
}

function Test-OwnerOnlyFileAcl([string]$Path) {
    $acl = Get-Acl -LiteralPath $Path
    if (-not $acl.AreAccessRulesProtected) { return $false }
    $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $allows = @($acl.Access)
    if ($allows.Count -lt 1) { return $false }
    $readable = $false
    foreach ($rule in $allows) {
        if ($rule.AccessControlType -ne 'Allow') { return $false }
        try { $ruleSid = Get-RuleSid $rule.IdentityReference } catch { return $false }
        if ($ruleSid -ne $sid) { return $false }
        if (($rule.FileSystemRights -band [System.Security.AccessControl.FileSystemRights]::ReadData) -ne 0) { $readable = $true }
    }
    return $readable
}

function Test-RegularFile([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return $false }
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.PSIsContainer) { return $false }
    if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { return $false }
    return $true
}

function Test-CurrentUserCanRead([string]$Path) {
    try { $acl = Get-Acl -LiteralPath $Path } catch { return $false }
    $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    foreach ($rule in @($acl.Access)) {
        if ($rule.AccessControlType -ne 'Allow') { continue }
        try { $ruleSid = Get-RuleSid $rule.IdentityReference } catch { continue }
        if ($ruleSid -eq $sid -and (($rule.FileSystemRights -band [System.Security.AccessControl.FileSystemRights]::ReadData) -ne 0)) {
            return $true
        }
    }
    return $false
}

function Repair-OwnerOnlyDescendants([string]$Path) {
    # Files created while the directory grant was not inheritable have an empty ACL.
    $account = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
    $pending = New-Object System.Collections.Generic.Queue[string]
    foreach ($child in @(Get-ChildItem -LiteralPath $Path -Force)) { $pending.Enqueue($child.FullName) }
    while ($pending.Count -gt 0) {
        $childPath = $pending.Dequeue()
        $item = Get-Item -LiteralPath $childPath -Force
        if (-not (Test-CurrentUserCanRead $childPath)) {
            $grant = if ($item.PSIsContainer) { '(OI)(CI)(F)' } else { '(R)' }
            & icacls.exe $childPath /grant:r "${account}:${grant}" | Out-Null
            if ($LASTEXITCODE -ne 0) { throw 'Could not restore access under .envtie.' }
        }
        if ($item.PSIsContainer) {
            foreach ($nested in @(Get-ChildItem -LiteralPath $childPath -Force)) { $pending.Enqueue($nested.FullName) }
        }
    }
}

function Initialize-OwnerOnlyDirectory([string]$Path) {
    if (Test-Path -LiteralPath $Path) {
        $item = Get-Item -LiteralPath $Path -Force
        if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw 'Refusing to use .envtie because it is not a regular directory.'
        }
    } else {
        New-Item -ItemType Directory -Path $Path | Out-Null
    }
    Set-OwnerOnlyFileAcl $Path
    if (-not (Test-OwnerOnlyFileAcl $Path)) { throw 'Could not restrict permissions on .envtie.' }
    Repair-OwnerOnlyDescendants $Path
}

function Read-TokenText([string]$Path) {
    $text = [System.IO.File]::ReadAllText($Path).Replace("`r", '')
    if ($text.EndsWith("`n")) { $text = $text.Substring(0, $text.Length - 1) }
    if ($text.Contains("`n")) { throw 'The runner token file is invalid.' }
    return $text
}

function Save-RunnerToken([string]$Path, [string]$Value) {
    $dir = Split-Path -Parent $Path
    Initialize-OwnerOnlyDirectory $dir
    if (Test-Path -LiteralPath $Path) {
        if (-not (Test-RegularFile $Path)) { throw 'Refusing to write the runner token because .envtie/runner.token is not a regular file.' }
    }
    $tmp = $Path + '.tmp'
    if (Test-Path -LiteralPath $tmp) {
        $tmpItem = Get-Item -LiteralPath $tmp -Force
        if ($tmpItem.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Refusing to write the runner token through a symlink.' }
        Remove-Item -LiteralPath $tmp -Force
    }
    [System.IO.File]::WriteAllText($tmp, $Value + "`n", [System.Text.UTF8Encoding]::new($false))
    try {
        Set-OwnerOnlyFileAcl $tmp
        if (-not (Test-OwnerOnlyFileAcl $tmp)) { throw 'Could not restrict permissions on .envtie/runner.token.' }
        Move-Item -LiteralPath $tmp -Destination $Path -Force
        Set-OwnerOnlyFileAcl $Path
        if (-not (Test-OwnerOnlyFileAcl $Path)) { throw 'Could not restrict permissions on .envtie/runner.token.' }
    } catch {
        Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
        if ((Test-Path -LiteralPath $Path) -and -not (Test-OwnerOnlyFileAcl $Path)) {
            Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue
        }
        throw
    }
}

function Save-RunnerTokenIfNeeded([string]$Value) {
    $shouldSave = $true
    if (Test-RegularFile $script:TokenPath) {
        if (Test-OwnerOnlyFileAcl $script:TokenPath) {
            try {
                $existing = Read-TokenText $script:TokenPath
                if ($existing -eq $Value) { $shouldSave = $false }
            } catch {
                $shouldSave = $true
            }
        }
    } elseif (Test-Path -LiteralPath $script:TokenPath) {
        throw 'Refusing to write the runner token because .envtie/runner.token is not a regular file.'
    }
    if ($shouldSave) {
        Save-RunnerToken $script:TokenPath $Value
        Write-Host 'Saved the runner token to .envtie/runner.token.'
    }
}

function Get-Ed25519PublicKey([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { throw 'Missing control plane public key at .envtie/controlplane-public.key. Start the local control plane first.' }
    if (-not (Test-RegularFile $Path)) { throw 'Refusing to read .envtie/controlplane-public.key because it is not a regular file.' }
    $text = ([System.IO.File]::ReadAllText($Path) -replace '\s', '')
    if (-not $text) { throw 'The control plane public key must be a base64-encoded Ed25519 public key.' }
    try { $bytes = [Convert]::FromBase64String($text) } catch { throw 'The control plane public key must be a base64-encoded Ed25519 public key.' }
    if ($bytes.Length -ne 32) { throw 'The control plane public key must be a base64-encoded Ed25519 public key.' }
    return $text
}

function Wait-ControlPlane([string]$Url) {
    $ProgressPreference = 'SilentlyContinue'
    $deadline = (Get-Date).AddSeconds(5)
    do {
        try {
            $response = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 3
            if ($response.StatusCode -eq 200 -and [string]$response.Content -match '"status"\s*:\s*"ok"') { return }
        } catch { }
        Start-Sleep -Milliseconds 400
    } while ((Get-Date) -lt $deadline)
    throw 'The control plane is not healthy at http://localhost:8080/healthz. Start it with .\scripts\start-local.ps1 first.'
}

Wait-ControlPlane 'http://localhost:8080/healthz'
Initialize-OwnerOnlyDirectory (Join-Path $root '.envtie')
$publicKey = Get-Ed25519PublicKey (Join-Path $root '.envtie/controlplane-public.key')
$null = Get-GoExe

$runnerToken = $null
$source = $null
if (-not [string]::IsNullOrWhiteSpace($env:ENVTIE_RUNNER_TOKEN)) {
    $runnerToken = $env:ENVTIE_RUNNER_TOKEN.Trim()
}
if ($null -ne $runnerToken) {
    if (-not (Test-RunnerToken $runnerToken)) { throw 'The runner token is invalid.' }
    $source = 'environment'
} elseif (Test-Path -LiteralPath $script:TokenPath) {
    if (-not (Test-RegularFile $script:TokenPath)) { throw 'Refusing to read .envtie/runner.token because it is not a regular file.' }
    if (-not (Test-OwnerOnlyFileAcl $script:TokenPath)) {
        throw 'Refusing to read .envtie/runner.token because its permissions are not limited to the current user. Delete the file and enter the token when prompted, or restrict the file to your user and run this script again.'
    }
    $runnerToken = Read-TokenText $script:TokenPath
    if (-not $runnerToken) { throw 'The runner token file is empty.' }
    if (-not (Test-RunnerToken $runnerToken)) { throw 'The runner token file is invalid.' }
    $source = 'file'
} else {
    Write-Host 'Register a runner in the dashboard (Runners > Register runner) and paste its token.'
    Write-Host 'The token is not accepted as a command-line argument.'
    $secure = Read-Host 'Runner token' -AsSecureString
    if ($secure.Length -lt 1) { throw 'A runner token is required.' }
    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
    try { $runnerToken = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr).Trim() }
    finally {
        [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
        $secure.Dispose()
    }
    if (-not $runnerToken) { throw 'A runner token is required.' }
    if (-not (Test-RunnerToken $runnerToken)) { throw 'The runner token is invalid.' }
    $source = 'prompt'
}
if ($source -ne 'file') { Save-RunnerTokenIfNeeded $runnerToken }

& (Join-Path $PSScriptRoot 'build.ps1')
foreach ($name in @('envtie.exe', 'adapter-neon.exe', 'adapter-upstash.exe', 'adapter-github.exe', 'adapter-stripe.exe')) {
    $artifact = Join-Path $root "dist/$name"
    if (-not (Test-Path -LiteralPath $artifact -PathType Leaf) -or (Get-Item -LiteralPath $artifact).Length -le 0) {
        throw "Missing adapter artifact: dist/$name"
    }
}
Write-Host 'Official adapters are in dist/ for local development. The control plane pins them in signed plans.'

# Credentials are not loaded from the repo .env. Each job asks the adapters in
# that plan which variables they need, including a custom adapter's own names.
Write-Host 'Provider credentials stay in this process and are not loaded from .env.'
Write-Host 'A job runs only the adapters in its plan. Each adapter checks the credentials it requires.'
Write-Host "Runner token: present ($source)"
Write-Host 'Starting the runner from the repository root.'

$saved = @{}
$runnerEnv = @{
    ENVTIE_API_URL      = 'http://localhost:8080'
    ENVTIE_RUNNER_TOKEN = $runnerToken
    ENVTIE_PUBLIC_KEY   = $publicKey
}
foreach ($key in @($runnerEnv.Keys)) {
    $saved[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
    [Environment]::SetEnvironmentVariable($key, [string]$runnerEnv[$key], 'Process')
}
Remove-Variable runnerToken, publicKey, runnerEnv
Push-Location $root
try {
    & (Join-Path $root 'dist\envtie.exe') runner execute
} finally {
    Pop-Location
    foreach ($key in @($saved.Keys)) {
        # PowerShell turns $null into "" for a string argument, which would leave an empty variable behind.
        if ($null -eq $saved[$key]) { Remove-Item -LiteralPath "Env:$key" -ErrorAction SilentlyContinue }
        else { [Environment]::SetEnvironmentVariable($key, $saved[$key], 'Process') }
    }
}
