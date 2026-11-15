# Armada agent installer for Windows. In an elevated PowerShell (Run as
# Administrator):
#
#   $env:ARMADA_SERVER_URL='https://fleet.example.com'; $env:ARMADA_ENROLLMENT_TOKEN='et_...'
#   irm https://fleet.example.com/install.ps1 | iex
#
# Downloads the newest armada-agent release for this platform from the
# server, verifies its SHA-256, installs it as the armada-agent service
# (started at boot, restarted if it stops) and enrolls. Running it again
# updates the binary and keeps the existing configuration.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is very slow with the progress bar

function Fail($msg) { throw "armada install: $msg" }

$server = $env:ARMADA_SERVER_URL
$token = $env:ARMADA_ENROLLMENT_TOKEN
if (-not $server) { Fail 'set $env:ARMADA_SERVER_URL to the Armada server URL' }
if ($server -notmatch '^https?://') { Fail 'ARMADA_SERVER_URL must start with https:// (or http://)' }
$server = $server.TrimEnd('/')

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
  [Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) { Fail 'run PowerShell as Administrator' }
# Windows PowerShell 5.1 may not offer TLS 1.2 by default.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { 'amd64' }
  'ARM64' { 'arm64' }
  'x86' { '386' }
  default { Fail "unsupported architecture $env:PROCESSOR_ARCHITECTURE" }
}

$installDir = Join-Path $env:ProgramFiles 'Armada'
$dataDir = Join-Path $env:ProgramData 'Armada'
$exe = Join-Path $installDir 'armada-agent.exe'
$conf = Join-Path $dataDir 'agent.json'
$log = Join-Path $dataDir 'agent.log'
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("armada-agent-" + [Guid]::NewGuid().ToString('N') + '.exe')

try {
  Write-Host "Downloading armada-agent for windows/$arch..."
  try {
    $res = Invoke-WebRequest -UseBasicParsing -Uri "$server/api/v1/agent/download?os=windows&arch=$arch" -OutFile $tmp -PassThru
  } catch {
    Fail "download failed; has an armada-agent release for windows/$arch been uploaded? ($($_.Exception.Message))"
  }
  $want = @($res.Headers['X-Armada-SHA256'])[0]
  $got = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash.ToLower()
  if (-not $want -or $want.ToLower() -ne $got) { Fail "checksum mismatch (expected $want, got $got)" }
  & $tmp -version | Out-Null
  if ($LASTEXITCODE -ne 0) { Fail 'the downloaded agent does not run on this machine' }

  # Only SYSTEM and Administrators may read the config and state (the
  # device key lives there).
  New-Item -ItemType Directory -Force -Path $installDir, $dataDir | Out-Null
  & icacls.exe $dataDir /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
  if ($LASTEXITCODE -ne 0) { Fail "could not restrict access to $dataDir" }

  if (-not (Test-Path $conf)) {
    if (-not $token) { Fail 'set $env:ARMADA_ENROLLMENT_TOKEN to an enrollment token' }
    $cfg = [ordered]@{
      server_url       = $server
      enrollment_token = $token
      state_dir        = (Join-Path $dataDir 'state')
      auto_update      = $true
      allow_reboot     = $true
      allow_exec       = $false
      log_files        = @{ agent = $log }
    }
    if ($env:ARMADA_DEVICE_NAME) { $cfg.name = $env:ARMADA_DEVICE_NAME }
    # UTF-8 without a byte-order mark (Set-Content -Encoding UTF8 adds one in 5.1).
    [IO.File]::WriteAllText($conf, ($cfg | ConvertTo-Json))
    Write-Host "Wrote $conf"
  } else {
    Write-Host "Keeping existing $conf"
  }

  $svc = Get-Service -Name 'armada-agent' -ErrorAction SilentlyContinue
  if ($svc -and $svc.Status -ne 'Stopped') {
    Stop-Service -Name 'armada-agent' -Force
    (Get-Service -Name 'armada-agent').WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
  }
  Copy-Item -Force $tmp $exe
  & $exe -install-service -config $conf -log-file $log
  if ($LASTEXITCODE -ne 0) { Fail 'could not install the service' }
  Start-Service -Name 'armada-agent'
  Write-Host "armada-agent installed and running. Logs: $log"
} finally {
  Remove-Item -Force -ErrorAction SilentlyContinue $tmp
}
