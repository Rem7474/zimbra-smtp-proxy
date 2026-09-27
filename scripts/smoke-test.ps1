# Test de fumée sur Windows : installation silencieuse, démarrage, dialogue
# SMTP sur 127.0.0.1:1025, puis désinstallation. Utilisé par la CI.
$ErrorActionPreference = "Stop"
$setup = Join-Path $PSScriptRoot "..\dist\ZimbraSmtpProxy-Setup.exe"
$appDir = Join-Path $env:LOCALAPPDATA "Programs\ZimbraSmtpProxy"
$exe = Join-Path $appDir "zimbra-smtp-proxy.exe"

Write-Host "Installation silencieuse"
Start-Process $setup -ArgumentList "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/TASKS=autostart" -Wait
if (-not (Test-Path $exe)) { throw "exe absent après installation : $exe" }
$run = Get-ItemProperty "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -Name ZimbraSmtpProxy -ErrorAction SilentlyContinue
if (-not $run) { throw "clé de démarrage automatique absente" }

Write-Host "Démarrage de l'application"
$proc = Start-Process $exe -PassThru
try {
  $client = $null
  for ($i = 0; $i -lt 30 -and -not $client; $i++) {
    try { $client = New-Object Net.Sockets.TcpClient("127.0.0.1", 1025) } catch { Start-Sleep -Seconds 1 }
  }
  if (-not $client) { throw "le port 1025 ne s'est pas ouvert" }
  $stream = $client.GetStream()
  $reader = New-Object IO.StreamReader($stream)
  $writer = New-Object IO.StreamWriter($stream); $writer.NewLine = "`r`n"; $writer.AutoFlush = $true
  $banner = $reader.ReadLine(); Write-Host "  $banner"
  if (-not $banner.StartsWith("220")) { throw "bannière SMTP inattendue" }
  $writer.WriteLine("EHLO ci")
  $ehlo = @()
  do { $line = $reader.ReadLine(); $ehlo += $line; Write-Host "  $line" } while ($line -match "^250-")
  if (-not ($ehlo -match "AUTH PLAIN")) { throw "AUTH PLAIN non annoncé" }
  $writer.WriteLine("MAIL FROM:<ci@example.org>")
  $resp = $reader.ReadLine(); Write-Host "  $resp"
  if ($resp.StartsWith("250")) { throw "MAIL accepté sans authentification" }
  $writer.WriteLine("QUIT")
  $client.Close()
} finally {
  Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
}

$log = Join-Path $env:APPDATA "ZimbraSmtpProxy\proxy.log"
if (Test-Path $log) { Write-Host "--- journal"; Get-Content $log }

Write-Host "Désinstallation silencieuse"
Start-Process (Join-Path $appDir "unins000.exe") -ArgumentList "/VERYSILENT", "/SUPPRESSMSGBOXES" -Wait
Start-Sleep -Seconds 2
if (Test-Path $exe) { throw "exe toujours présent après désinstallation" }
if (Get-ItemProperty "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -Name ZimbraSmtpProxy -ErrorAction SilentlyContinue) {
  throw "clé de démarrage automatique toujours présente"
}
Write-Host "Test de fumée OK"
