# NestNAS installer for Windows (PowerShell):
#   irm https://raw.githubusercontent.com/Saransh-Jainbu/nestnas/main/install.ps1 | iex
$ErrorActionPreference = "Stop"
$repo = if ($env:NESTNAS_REPO) { $env:NESTNAS_REPO } else { "Saransh-Jainbu/nestnas" }
$dir  = Join-Path $env:LOCALAPPDATA "NestNAS"
New-Item -ItemType Directory -Force $dir | Out-Null
$exe  = Join-Path $dir "nestnas.exe"
$base = "https://github.com/$repo/releases/latest/download"
$tmp  = Join-Path $env:TEMP "nestnas-install"
New-Item -ItemType Directory -Force $tmp | Out-Null
Write-Host "Downloading NestNAS..."
Invoke-WebRequest "$base/nestnas-windows-amd64.exe" -OutFile "$tmp\nestnas.exe"
Invoke-WebRequest "$base/SHA256SUMS" -OutFile "$tmp\SHA256SUMS"
$want = ((Get-Content "$tmp\SHA256SUMS" | Where-Object { $_ -match "nestnas-windows-amd64.exe$" }) -split "\s+")[0]
$got  = (Get-FileHash "$tmp\nestnas.exe" -Algorithm SHA256).Hash.ToLower()
if (-not $want -or $got -ne $want.ToLower()) { throw "Checksum mismatch. Nothing was installed." }
Copy-Item "$tmp\nestnas.exe" $exe -Force
Write-Host "Installed to $exe"
& $exe service install
