# run-bench-windows.ps1 — Geekflare-style benchmark rig for Codvian Router, Windows port
#
# Faithful port of scripts/bench-geekflare.sh (CHECKLIST 149/150/207/208).
# Method: two instant mock upstreams (codvian-bench mock, zero delay),
# added latency = median of 1,000 sequential POSTs through the router minus
# the median of 1,000 sequential POSTs direct to the mock; RPS = 32 clients
# x 20 s through the router and direct to one mock (the direct run is the
# canary: a pass whose canary drops far below the other pass's canary is
# contended and must be reported as polluted); idle RSS after a 3 s settle;
# peak RSS sampled every 100 ms during the router RPS runs; ready time =
# spawn to first HTTP 200 on /healthz and on /v1/models. Whole sequence
# runs twice (pass 1 = reported run, pass 2 = stability check).
#
# Executor: the AGENT over SSH (Tailscale + Windows OpenSSH Server), after
# Raunak approves the one-time pairing. Non-interactive by design: no
# prompts, no GUI, everything captured to files under -WorkDir.
# STATUS (2026-11-19): pairing DECLINED for now — this script is HELD
# preparation, not to be run on Raunak's PC until he approves pairing.
#
# Usage (over SSH, from the package directory):
#   powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\run-bench-windows.ps1 -WorkDir C:\codvian-bench\run-2026-11-19
# Parameters:
#   -WorkDir     where all outputs land (created if missing). Default: $env:TEMP\codvian-bench-windows
#   -LimiterMode on|off. Default on. off sets CODVIAN_ROUTER_NO_ADAPTIVE_LIMITER=1 (E1 mode; the
#                E1 verdict is already measured — do not use off for the 149/150/207/208 re-run).
#   -BenchOut    optional extra path to copy bench-results.json to.
#   -SkipDashboardCheck  skip the final GET /dashboard/benchmark live check (items 150/208).
#
# Exit code: 0 = run completed (numbers may still MISS targets — a miss is a
# result, not a failure); 1 = the rig itself failed; 2 = preflight refused
# (machine not quiet). A polluted pass does NOT fail the script — it is
# recorded as polluted in bench-results.json and must be reported as such.

#Requires -Version 5.1
param(
    [string]$WorkDir = (Join-Path $env:TEMP 'codvian-bench-windows'),
    [ValidateSet('on','off')] [string]$LimiterMode = 'on',
    [string]$BenchOut = '',
    [switch]$SkipDashboardCheck
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$BenchExe  = Join-Path $ScriptDir 'codvian-bench.exe'
$RouterExe = Join-Path $ScriptDir 'codvian-router.exe'
$LogDir    = Join-Path $WorkDir 'logs'
$HomeDir   = Join-Path $WorkDir 'home'
$ConfigPath = Join-Path $WorkDir 'config.json'
$KeyFile   = Join-Path $WorkDir 'gateway.key'
$DataDir   = Join-Path $HomeDir '.local\share\codvian-router'

$MockA = $null; $MockB = $null; $RouterProc = $null
$OldUserProfile = $env:USERPROFILE

function Get-FreePort {
    $l = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, 0)
    $l.Start()
    $p = $l.LocalEndpoint.Port
    $l.Stop()
    return $p
}

function Get-CpuAvgPct {
    # Average total-CPU % over 5 one-second samples (rough Windows analogue
    # of the VM's loadavg check; NOT equivalent — recorded, not hidden).
    $samples = (Get-Counter '\Processor(_Total)\% Processor Time' -SampleInterval 1 -MaxSamples 5).CounterSamples |
        ForEach-Object { $_.CookedValue }
    return [math]::Round(($samples | Measure-Object -Average).Average, 1)
}

function Get-RssKb([int]$Pid) {
    try { return [math]::Round((Get-Process -Id $Pid -ErrorAction Stop).WorkingSet64 / 1KB) }
    catch { return $null }
}

function Wait-Http200([string]$Url, [string]$AuthHeader, [System.Diagnostics.Stopwatch]$Sw) {
    # Polls until HTTP 200 (or 30 s); returns elapsed ms since $Sw started, or $null on timeout.
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
        try {
            $headers = @{}
            if ($AuthHeader) { $headers['Authorization'] = $AuthHeader }
            $r = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 2 -Headers $headers -ErrorAction Stop
            if ($r.StatusCode -eq 200) { return [math]::Round($Sw.Elapsed.TotalMilliseconds, 1) }
        } catch { }
        Start-Sleep -Milliseconds 5
    }
    return $null
}

function Stop-StartedProcesses {
    if ($RouterProc -and -not $RouterProc.HasExited) { try { $RouterProc.Kill() } catch { } }
    foreach ($m in @($MockA, $MockB)) { if ($m -and -not $m.HasExited) { try { $m.Kill() } catch { } } }
    $script:RouterProc = $null; $script:MockA = $null; $script:MockB = $null
}

try {
    New-Item -ItemType Directory -Force -Path $WorkDir, $LogDir, $HomeDir | Out-Null
    $transcript = Join-Path $LogDir 'run-transcript.txt'
    Start-Transcript -Path $transcript -Force | Out-Null

    if (-not (Test-Path $BenchExe))  { Write-Error "missing $BenchExe"; exit 1 }
    if (-not (Test-Path $RouterExe)) { Write-Error "missing $RouterExe"; exit 1 }

    # --- rig info ---
    $cpuModel = (Get-CimInstance Win32_Processor | Select-Object -First 1).Name
    $memTotalKib = [math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory / 1KB)
    $osCaption = (Get-CimInstance Win32_OperatingSystem).Caption
    $goVersion = 'not installed (prebuilt binaries used)'
    try { $goVersion = (& go version 2>$null) } catch { }
    $routerVersion = (& $RouterExe version 2>$null)
    Write-Host "rig: $osCaption, CPUs=$([Environment]::ProcessorCount), mem=${memTotalKib} KiB, cpu=$cpuModel"
    Write-Host "router: $routerVersion, go: $goVersion, limiter: $LimiterMode"

    # --- preflight: quiet machine (Windows analogue of the VM's loadavg < 3 rule) ---
    $goProcs = Get-Process go -ErrorAction SilentlyContinue
    if ($goProcs) {
        Write-Error "preflight refused: go.exe is running (another build/test) — aborting, do not bench into a storm"
        exit 2
    }
    $cpuStart = Get-CpuAvgPct
    Write-Host "cpu avg at start: $cpuStart% (5 x 1 s samples)"
    if ($cpuStart -gt 30) {
        Write-Error "preflight refused: CPU avg $cpuStart% > 30% — machine is not quiet, aborting"
        exit 2
    } elseif ($cpuStart -gt 10) {
        Write-Host "WARNING: CPU avg $cpuStart% is 10-30% — machine is not fully idle; the run proceeds but the numbers must be reported with this caveat"
    }

    $MockPort  = Get-FreePort
    $MockPort2 = Get-FreePort
    $RouterPort = Get-FreePort
    Write-Host "mock ports: $MockPort $MockPort2 (instant, zero delay), router port: $RouterPort"

    if ($LimiterMode -eq 'off') { $env:CODVIAN_ROUTER_NO_ADAPTIVE_LIMITER = '1' }

    # --- mocks ---
    $MockA = Start-Process $BenchExe -ArgumentList 'mock','--port',"$MockPort" `
        -RedirectStandardOutput (Join-Path $LogDir 'mock-a.txt') `
        -RedirectStandardError  (Join-Path $LogDir 'mock-a.err.txt') `
        -WindowStyle Hidden -PassThru
    $MockB = Start-Process $BenchExe -ArgumentList 'mock','--port',"$MockPort2" `
        -RedirectStandardOutput (Join-Path $LogDir 'mock-b.txt') `
        -RedirectStandardError  (Join-Path $LogDir 'mock-b.err.txt') `
        -WindowStyle Hidden -PassThru
    Start-Sleep -Milliseconds 500
    Invoke-WebRequest -Uri "http://127.0.0.1:$MockPort/v1/models"  -UseBasicParsing -TimeoutSec 5 | Out-Null
    Invoke-WebRequest -Uri "http://127.0.0.1:$MockPort2/v1/models" -UseBasicParsing -TimeoutSec 5 | Out-Null
    Write-Host "mock upstreams up on 127.0.0.1:$MockPort and 127.0.0.1:$MockPort2"

    # --- fresh isolated config + data dir (never touches an existing install) ---
    # The router resolves its data dir from %USERPROFILE% and its config from
    # --config, so both are pointed at the work dir for the child processes.
    $env:USERPROFILE = $HomeDir
    $initOut = & $RouterExe --config $ConfigPath init --key-file $KeyFile 2>&1
    $initOut | Out-File (Join-Path $LogDir 'init.txt')
    if (-not (Test-Path $KeyFile)) { Write-Error "init failed (no key file); see logs\init.txt"; exit 1 }
    $Key = (Get-Content $KeyFile -Raw).Trim()
    if (-not $Key) { Write-Error "init produced an empty key file"; exit 1 }

    & $RouterExe --config $ConfigPath provider add "http://127.0.0.1:$MockPort"  mock-key-a --name mock-a  *> (Join-Path $LogDir 'provider-a.txt')
    & $RouterExe --config $ConfigPath provider add "http://127.0.0.1:$MockPort2" mock-key-b --name mock-b *> (Join-Path $LogDir 'provider-b.txt')
    Write-Host "providers: mock-a (127.0.0.1:$MockPort), mock-b (127.0.0.1:$MockPort2), model model-a"

    # --- router ---
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $RouterProc = Start-Process $RouterExe -ArgumentList '--config', $ConfigPath, '--port', "$RouterPort", 'serve' `
        -RedirectStandardOutput (Join-Path $LogDir 'router.txt') `
        -RedirectStandardError  (Join-Path $LogDir 'router.err.txt') `
        -WindowStyle Hidden -PassThru
    $readyHealthz = Wait-Http200 "http://127.0.0.1:$RouterPort/healthz" '' $sw
    $readyModels  = Wait-Http200 "http://127.0.0.1:$RouterPort/v1/models" "Bearer $Key" $sw
    if ($null -eq $readyHealthz -or $null -eq $readyModels) {
        Write-Error "router did not become ready within 30 s; see logs\router.txt and logs\router.err.txt"
        exit 1
    }
    Write-Host "ready: /healthz ${readyHealthz} ms, /v1/models ${readyModels} ms (spawn to first 200)"
    Start-Sleep -Seconds 3  # settle: idle RSS
    $idleRssKb = Get-RssKb $RouterProc.Id
    Write-Host "idle RSS: $([math]::Round($idleRssKb / 1024, 1)) MiB ($idleRssKb KiB)"

    $passes = @{}
    foreach ($pass in 1, 2) {
        Write-Host ""
        Write-Host "== pass $pass =="
        $latDirect = & $BenchExe latency --base "http://127.0.0.1:$MockPort" --model model-a --requests 1000 --warmup 25 |
            Out-String | ConvertFrom-Json
        $latDirect | ConvertTo-Json -Depth 5 | Set-Content (Join-Path $WorkDir "lat-direct-$pass.json")
        Write-Host "direct latency: $($latDirect | ConvertTo-Json -Compress)"
        $latRouter = & $BenchExe latency --base "http://127.0.0.1:$RouterPort" --key $Key --model model-a --requests 1000 --warmup 25 |
            Out-String | ConvertFrom-Json
        $latRouter | ConvertTo-Json -Depth 5 | Set-Content (Join-Path $WorkDir "lat-router-$pass.json")
        Write-Host "router latency: $($latRouter | ConvertTo-Json -Compress)"

        $rpsDirect = & $BenchExe rps --base "http://127.0.0.1:$MockPort" --model model-a --concurrency 32 --duration 20s --warmup 25 |
            Out-String | ConvertFrom-Json
        $rpsDirect | ConvertTo-Json -Depth 5 | Set-Content (Join-Path $WorkDir "rps-direct-$pass.json")
        Write-Host "direct rps: $($rpsDirect | ConvertTo-Json -Compress)"

        # Peak RSS sampler during the router RPS run (100 ms samples, bytes).
        $peakFile = Join-Path $WorkDir "peak-$pass.txt"
        Set-Content $peakFile $idleRssKb
        $sampler = Start-Job -ScriptBlock {
            param($PidToWatch, $File)
            while ($true) {
                $p = Get-Process -Id $PidToWatch -ErrorAction SilentlyContinue
                if (-not $p) { break }
                Add-Content -Path $File -Value ([math]::Round($p.WorkingSet64 / 1KB))
                Start-Sleep -Milliseconds 100
            }
        } -ArgumentList $RouterProc.Id, $peakFile
        $rpsRouter = & $BenchExe rps --base "http://127.0.0.1:$RouterPort" --key $Key --model model-a --concurrency 32 --duration 20s --warmup 25 |
            Out-String | ConvertFrom-Json
        Stop-Job $sampler | Out-Null
        Remove-Job $sampler -Force | Out-Null
        $rpsRouter | ConvertTo-Json -Depth 5 | Set-Content (Join-Path $WorkDir "rps-router-$pass.json")
        $peakKb = (Get-Content $peakFile | ForEach-Object { [int]$_ } | Measure-Object -Maximum).Maximum
        Set-Content (Join-Path $WorkDir "peak-kb-$pass.txt") $peakKb
        Write-Host "router rps: $($rpsRouter | ConvertTo-Json -Compress)"
        Write-Host "peak RSS during router rps (pass $pass): $([math]::Round($peakKb / 1024, 1)) MiB ($peakKb KiB)"

        $passes["$pass"] = [ordered]@{
            direct_latency = $latDirect
            router_latency = $latRouter
            added_latency_p50_ms = [math]::Round($latRouter.p50_ms - $latDirect.p50_ms, 3)
            direct_rps = $rpsDirect
            router_rps = $rpsRouter
            peak_rss_kib = $peakKb
        }
    }

    $cpuEnd = Get-CpuAvgPct
    Write-Host "cpu avg at end: $cpuEnd%"

    Stop-StartedProcesses

    # --- merge ---
    $result = [ordered]@{
        limiter_mode = $LimiterMode
        rig = [ordered]@{
            os = "$osCaption"
            cpus = [Environment]::ProcessorCount
            mem_total_kib = $memTotalKib
            cpu_model = $cpuModel
            go_version = $goVersion
            router_version = "$routerVersion"
            cpu_pct_start = $cpuStart
            cpu_pct_end = $cpuEnd
            date = (Get-Date -Format o)
            note = 'Windows run on Raunak''s PC (agent over SSH). Quiet-window rule: no go.exe running, CPU avg <= 30% at start. Direct-mock RPS is the canary: a pass whose canary drops far below the other pass''s canary is contended/polluted — no VM-calibrated hard threshold applies on this machine.'
        }
        ready_ms = [ordered]@{ healthz = $readyHealthz; v1_models = $readyModels }
        idle_rss_kib = $idleRssKb
        passes = $passes
    }
    $resultPath = Join-Path $WorkDir 'bench-results.json'
    $result | ConvertTo-Json -Depth 12 | Set-Content $resultPath
    if ($BenchOut) {
        try { Copy-Item $resultPath $BenchOut -Force; Write-Host "benchmark copy: $BenchOut" }
        catch { Write-Host "benchmark copy failed ($BenchOut): $_" }
    }

    # --- dashboard live check (items 150/208) ---
    if (-not $SkipDashboardCheck) {
        New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
        Copy-Item $resultPath (Join-Path $DataDir 'benchmark.json') -Force
        $sw2 = [System.Diagnostics.Stopwatch]::StartNew()
        $RouterProc = Start-Process $RouterExe -ArgumentList '--config', $ConfigPath, '--port', "$RouterPort", 'serve' `
            -RedirectStandardOutput (Join-Path $LogDir 'router-dashboard.txt') `
            -RedirectStandardError  (Join-Path $LogDir 'router-dashboard.err.txt') `
            -WindowStyle Hidden -PassThru
        $null = Wait-Http200 "http://127.0.0.1:$RouterPort/healthz" '' $sw2
        try {
            $feed = Invoke-WebRequest -Uri "http://127.0.0.1:$RouterPort/dashboard/benchmark" -UseBasicParsing -TimeoutSec 10
            $feed.Content | Set-Content (Join-Path $LogDir 'dashboard-benchmark.json')
            Write-Host "dashboard feed: HTTP $($feed.StatusCode); body saved to logs\dashboard-benchmark.json"
        } catch {
            Write-Host "dashboard feed check FAILED: $_ (recorded, not hidden — see run-transcript.txt)"
            "DASHBOARD FEED CHECK FAILED: $_" | Set-Content (Join-Path $LogDir 'dashboard-benchmark.json')
        }
        try {
            $dash = Invoke-WebRequest -Uri "http://127.0.0.1:$RouterPort/dashboard" -UseBasicParsing -TimeoutSec 10
            $dash.Content | Set-Content (Join-Path $LogDir 'dashboard.html')
            $hasBenchmark = $dash.Content -match 'Benchmark'
            Write-Host "dashboard page: HTTP $($dash.StatusCode); Benchmark section present: $hasBenchmark"
        } catch {
            Write-Host "dashboard page check FAILED: $_"
        }
        Stop-StartedProcesses
    }

    # --- summary table ---
    $summary = @()
    foreach ($p in 1, 2) {
        $d = $passes["$p"]
        $summary += "== pass $p summary =="
        $summary += "added latency (router p50 - direct p50): $($d.added_latency_p50_ms) ms (router p50 $($d.router_latency.p50_ms) ms, direct p50 $($d.direct_latency.p50_ms) ms; router cache_hits $($d.router_latency.cache_hits)/$($d.router_latency.ok), errors $($d.router_latency.errors))"
        $summary += "router RPS (32 clients x 20 s): $($d.router_rps.rps) (upstream $($d.router_rps.upstream_rps), ok $($d.router_rps.ok), errors $($d.router_rps.errors), cache_hits $($d.router_rps.cache_hits)); direct mock RPS: $($d.direct_rps.rps)"
    }
    $summary += "ready: /healthz $readyHealthz ms, /v1/models $readyModels ms"
    $summary += "idle RSS: $([math]::Round($idleRssKb / 1024, 1)) MiB; peak RSS pass 1: $([math]::Round($passes['1'].peak_rss_kib / 1024, 1)) MiB, pass 2: $([math]::Round($passes['2'].peak_rss_kib / 1024, 1)) MiB"
    $summary += "cpu avg: $cpuStart% at start, $cpuEnd% at end"
    $summary += "combined results: $resultPath"
    $summary | ForEach-Object { Write-Host $_ }
    $summary | Set-Content (Join-Path $WorkDir 'summary.txt')

    Write-Host "logs kept in $LogDir"
    Stop-Transcript | Out-Null
    exit 0
}
catch {
    Write-Host "run-bench-windows FAILED: $_"
    try { Stop-Transcript | Out-Null } catch { }
    exit 1
}
finally {
    Stop-StartedProcesses
    if ($OldUserProfile) { $env:USERPROFILE = $OldUserProfile }
    Remove-Item Env:\CODVIAN_ROUTER_NO_ADAPTIVE_LIMITER -ErrorAction SilentlyContinue
}
