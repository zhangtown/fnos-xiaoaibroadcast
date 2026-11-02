# 生成小爱播报的应用图标（飞牛风格：圆角方块 + 白色喇叭）。
# 一次产出 ICON.PNG(64) / ICON_256.PNG(256) / app/ui/images/icon_{64,256}.png / icon.png(128)
# 用 GDI+ 画（不依赖 Go 编译器，也不产生新的 exe）。
Add-Type -AssemblyName System.Drawing

$root = Split-Path -Parent $PSScriptRoot
$pkg  = Join-Path $root 'packaging\fnos\xiaoai-broadcast'
$ui   = Join-Path $pkg 'app\ui\images'
New-Item -ItemType Directory -Force -Path $ui | Out-Null

function New-Brush {
    param([System.Drawing.RectangleF]$Rect)
    $c1 = [System.Drawing.Color]::FromArgb(255, 255, 158, 92)
    $c2 = [System.Drawing.Color]::FromArgb(255, 255, 84, 40)
    $b = New-Object System.Drawing.Drawing2D.LinearGradientBrush($Rect, $c1, $c2, 55.0)
    $blend = New-Object System.Drawing.Drawing2D.ColorBlend
    $blend.Colors = @($c1, $c2)
    $blend.Positions = @(0.0, 1.0)
    $b.InterpolationColors = $blend
    return $b
}

function Add-RoundedRect {
    param($Path, [single]$x, [single]$y, [single]$w, [single]$h, [single]$r)
    $d = $r * 2
    $Path.AddArc($x, $y, $d, $d, 180, 90)
    $Path.AddArc($x + $w - $d, $y, $d, $d, 270, 90)
    $Path.AddArc($x + $w - $d, $y + $h - $d, $d, $d, 0, 90)
    $Path.AddArc($x, $y + $h - $d, $d, $d, 90, 90)
    $Path.CloseFigure()
}

function New-Master {
    param([int]$Size)
    $bmp = New-Object System.Drawing.Bitmap($Size, $Size, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
    $g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
    $g.Clear([System.Drawing.Color]::Transparent)

    $s = [single]$Size
    $rect = New-Object System.Drawing.RectangleF(0, 0, $s, $s)

    # 背景：圆角方块 + 渐变
    $bg = New-Object System.Drawing.Drawing2D.GraphicsPath
    Add-RoundedRect $bg 0 0 $s $s ($s * 0.225)
    $brush = New-Brush $rect
    $g.FillPath($brush, $bg)

    # 左上角一团很淡的暖光，避免纯渐变显得死板（无硬边）
    $g.SetClip($bg)
    $glow = New-Object System.Drawing.Drawing2D.GraphicsPath
    $glow.AddEllipse(($s * -0.15), ($s * -0.28), ($s * 0.95), ($s * 0.95))
    $glowBrush = New-Object System.Drawing.Drawing2D.PathGradientBrush($glow)
    $glowBrush.CenterColor = [System.Drawing.Color]::FromArgb(64, 255, 230, 210)
    $glowBrush.SurroundColors = @([System.Drawing.Color]::FromArgb(0, 255, 230, 210))
    $g.FillPath($glowBrush, $glow)
    $g.ResetClip()

    # 喇叭：音腔 + 锥形
    $white = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::White)
    $body = New-Object System.Drawing.Drawing2D.GraphicsPath
    Add-RoundedRect $body ($s * 0.245) ($s * 0.405) ($s * 0.135) ($s * 0.19) ($s * 0.038)
    $g.FillPath($white, $body)

    $cone = New-Object System.Drawing.Drawing2D.GraphicsPath
    [System.Drawing.PointF[]]$pts = @(
        (New-Object System.Drawing.PointF(($s * 0.365), ($s * 0.405))),
        (New-Object System.Drawing.PointF(($s * 0.545), ($s * 0.305))),
        (New-Object System.Drawing.PointF(($s * 0.545), ($s * 0.695))),
        (New-Object System.Drawing.PointF(($s * 0.365), ($s * 0.595)))
    )
    $cone.AddPolygon($pts)
    $g.FillPath($white, $cone)

    # 两道声波（圆环的一段弧）
    $pen = New-Object System.Drawing.Pen([System.Drawing.Color]::White, ($s * 0.036))
    $pen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $pen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    foreach ($r in @(0.095, 0.155)) {
        $rr = $s * $r
        $cx = $s * 0.555
        $cy = $s * 0.5
        $g.DrawArc($pen, ($cx - $rr), ($cy - $rr), ($rr * 2), ($rr * 2), -52, 104)
    }

    $g.Dispose()
    return $bmp
}

function Save-Scaled {
    param($Src, [int]$Size, [string]$Path)
    $out = New-Object System.Drawing.Bitmap($Size, $Size, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
    $g = [System.Drawing.Graphics]::FromImage($out)
    $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
    $g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
    $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::HighQuality
    $g.DrawImage($Src, (New-Object System.Drawing.Rectangle(0, 0, $Size, $Size)))
    $g.Dispose()
    $out.Save($Path, [System.Drawing.Imaging.ImageFormat]::Png)
    $out.Dispose()
}

$master = New-Master 1024
Save-Scaled $master 256 (Join-Path $pkg 'ICON_256.PNG')
Save-Scaled $master 256 (Join-Path $ui 'icon_256.png')
Save-Scaled $master 64  (Join-Path $ui 'icon_64.png')
# 注意：Windows 文件名不区分大小写，`ICON.PNG` 与 `icon.png` 是同一个文件，
# 所以这个包里不放 icon.png（网页标签图标用内联 SVG data URI）。
# ICON.PNG 必须最后写，且必须是 64。
Save-Scaled $master 64  (Join-Path $pkg 'ICON.PNG')
$master.Dispose()

Get-ChildItem $pkg -Filter '*.PNG' | Select-Object Name, Length | Format-Table -AutoSize
Get-ChildItem $ui | Select-Object Name, Length | Format-Table -AutoSize
