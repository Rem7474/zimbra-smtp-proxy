# Compile l'exe et l'installeur sous Windows.
# Prérequis : Go (version de go.mod), Inno Setup 6 (https://jrsoftware.org/isdl.php)
# Usage : .\build.ps1 [-Version 1.2.3]
param([string]$Version = "0.0.0")
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force dist | Out-Null

function Invoke-Step([scriptblock]$Cmd) {
  & $Cmd
  if ($LASTEXITCODE) { throw "échec : $Cmd" }
}

Invoke-Step { go test ./... }
Invoke-Step { go run ./tools/genicon }
Invoke-Step { go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64 --manifest gui `
  --icon winres/icon.png --product-name "Zimbra SMTP Proxy" `
  --file-description "Passerelle SMTP locale vers Zimbra" `
  --product-version $Version --file-version $Version `
  --original-filename zimbra-smtp-proxy.exe }
$env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
Invoke-Step { go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$Version" -o dist/zimbra-smtp-proxy.exe . }
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

$iscc = (Get-Command iscc -ErrorAction SilentlyContinue).Source
if (-not $iscc) {
  $iscc = @("${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe", "$env:ProgramFiles\Inno Setup 6\ISCC.exe",
            "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe") | Where-Object { Test-Path $_ } | Select-Object -First 1
}
if (-not $iscc) { throw "Inno Setup 6 introuvable" }
Invoke-Step { & $iscc /Q "/DAppVersion=$Version" installer\setup.iss }
Write-Host "-> dist\zimbra-smtp-proxy.exe, dist\ZimbraSmtpProxy-Setup.exe"
