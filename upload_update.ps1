param(
    [string]$ServerIP = "localhost",
    [int]$Port = 8081,
    [string]$User = "admin",
    [string]$Pass = "admin",
    [Parameter(Mandatory=$true)]
    [string]$Version,
    [string]$Description = "Update",
    [string]$BinaryPath = "client.exe"
)

Write-Host "🚀 Tunnel Client Update Uploader" -ForegroundColor Cyan
Write-Host "================================" -ForegroundColor Cyan
Write-Host ""

# Check if binary exists
if (-not (Test-Path $BinaryPath)) {
    Write-Host "❌ Error: Binary file not found: $BinaryPath" -ForegroundColor Red
    exit 1
}

$fileSize = (Get-Item $BinaryPath).Length
Write-Host "📦 Binary: $BinaryPath ($([math]::Round($fileSize/1MB, 2)) MB)" -ForegroundColor Yellow
Write-Host "🏷️  Version: $Version" -ForegroundColor Yellow
Write-Host "📝 Description: $Description" -ForegroundColor Yellow
Write-Host "🌐 Server: http://${ServerIP}:${Port}" -ForegroundColor Yellow
Write-Host ""

$url = "http://${ServerIP}:${Port}/api/update/upload"

# Create basic auth header
$pair = "${User}:${Pass}"
$bytes = [System.Text.Encoding]::ASCII.GetBytes($pair)
$base64 = [System.Convert]::ToBase64String($bytes)
$headers = @{
    Authorization = "Basic $base64"
}

# Read binary file
$binaryContent = [System.IO.File]::ReadAllBytes($BinaryPath)

# Create multipart form data
$boundary = [System.Guid]::NewGuid().ToString()
$LF = "`r`n"

$bodyLines = @()
$bodyLines += "--$boundary"
$bodyLines += "Content-Disposition: form-data; name=`"version`"$LF"
$bodyLines += $Version
$bodyLines += "--$boundary"
$bodyLines += "Content-Disposition: form-data; name=`"description`"$LF"
$bodyLines += $Description
$bodyLines += "--$boundary"
$bodyLines += "Content-Disposition: form-data; name=`"binary`"; filename=`"client.exe`""
$bodyLines += "Content-Type: application/octet-stream$LF"

$bodyString = $bodyLines -join $LF
$bodyBytes = [System.Text.Encoding]::UTF8.GetBytes($bodyString)

# Combine all parts
$endBoundary = [System.Text.Encoding]::UTF8.GetBytes("$LF--$boundary--$LF")
$fullBody = $bodyBytes + $binaryContent + $endBoundary

$headers["Content-Type"] = "multipart/form-data; boundary=$boundary"

Write-Host "⬆️  Uploading..." -ForegroundColor Cyan

try {
    $response = Invoke-RestMethod -Uri $url -Method Post -Headers $headers -Body $fullBody
    Write-Host ""
    Write-Host "✅ Upload successful!" -ForegroundColor Green
    Write-Host "   Version: $($response.version)" -ForegroundColor Green
    Write-Host "   Size: $($response.size) bytes" -ForegroundColor Green
    Write-Host ""
    Write-Host "🎉 Clients will auto-update within 30 minutes!" -ForegroundColor Cyan
} catch {
    Write-Host ""
    Write-Host "❌ Upload failed!" -ForegroundColor Red
    Write-Host "   Error: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host ""
    if ($_.Exception.Response) {
        $statusCode = $_.Exception.Response.StatusCode.value__
        Write-Host "   HTTP Status: $statusCode" -ForegroundColor Red
    }
    exit 1
}
