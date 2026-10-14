param([string]$Destination = '')
$ErrorActionPreference = 'Stop'
if (-not $Destination) {
    $base = if (Test-Path -LiteralPath (Join-Path $PSScriptRoot 'go.mod')) { Join-Path $PSScriptRoot 'dist' } else { $PSScriptRoot }
    $Destination = Join-Path $base 'stt'
}
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
New-Item -ItemType Directory -Force -Path $Destination | Out-Null
function Get-VerifiedFile([string]$Url, [string]$Path, [string]$Hash) {
    if ((Test-Path -LiteralPath $Path) -and (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash -eq $Hash) { return }
    Write-Host "Downloading $([IO.Path]::GetFileName($Path))..."
    $part = "$Path.partial"
    Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $part
    if ((Get-FileHash -LiteralPath $part -Algorithm SHA256).Hash -ne $Hash) { throw "SHA256 mismatch: $Path" }
    Move-Item -LiteralPath $part -Destination $Path -Force
}
# Pinned community build of upstream whisper.cpp with Vulkan (AMD/Intel/NVIDIA).
# Build recipe: github.com/jiang1997/whisper.cpp-release/tree/v1.8.4.1/.github/workflows
$zip = Join-Path $Destination 'whisper-vulkan.zip'
Get-VerifiedFile 'https://github.com/jiang1997/whisper.cpp-release/releases/download/v1.8.4.1/whisper-1.8.4-windows-x64.zip' $zip '4b1b36343feb55ec3deace6a7dd18cc217f43a55e4ecce76ccd4ee3595c0b642'
Expand-Archive -LiteralPath $zip -DestinationPath $Destination -Force
Copy-Item -LiteralPath (Join-Path $Destination 'whisper-1.8.4-windows-x64\whisper-server.exe') -Destination $Destination -Force
Get-VerifiedFile 'https://huggingface.co/ggerganov/whisper.cpp/resolve/98aa99a0a9db05ae2342309f5096248665f7cba3/ggml-large-v3-turbo-q5_0.bin' (Join-Path $Destination 'ggml-large-v3-turbo-q5_0.bin') '394221709cd5ad1f40c46e6031ca61bce88931e6e088c188294c6d5a55ffa7e2'
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'THIRD_PARTY_NOTICES.txt') -Destination (Join-Path $Destination 'THIRD_PARTY_NOTICES.txt') -Force
Write-Host "Ready: $Destination"
