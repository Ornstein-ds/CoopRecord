$ErrorActionPreference = 'Stop'
# Portable official Go SDK; no administrator rights or global PATH changes.
$toolsDir = Join-Path $PSScriptRoot '.tools'
$goTool = Join-Path $toolsDir 'go/bin/go.exe'
if (Test-Path -LiteralPath $goTool) {
    & $goTool version
    exit $LASTEXITCODE
}
New-Item -ItemType Directory -Force -Path $toolsDir | Out-Null
$releases = Invoke-RestMethod -Uri 'https://go.dev/dl/?mode=json'
$release = $releases | Where-Object stable | Select-Object -First 1
$file = $release.files | Where-Object { $_.os -eq 'windows' -and $_.arch -eq 'amd64' -and $_.kind -eq 'archive' }
if (-not $file) { throw 'Official Windows x64 Go archive was not found.' }
$archivePath = Join-Path $toolsDir 'go.zip'
Invoke-WebRequest -UseBasicParsing -Uri ('https://go.dev/dl/' + $file.filename) -OutFile $archivePath
if ((Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant() -ne $file.sha256) {
    throw 'Go archive checksum mismatch. Installation stopped.'
}
Expand-Archive -LiteralPath $archivePath -DestinationPath $toolsDir -Force
& $goTool version
if ($LASTEXITCODE -ne 0) { throw 'Go installation failed.' }
Write-Output 'Go installed. Run .\build.ps1 to build CoopRecord.'
