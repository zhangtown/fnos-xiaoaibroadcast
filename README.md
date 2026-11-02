# 小爱播报（xiaoai-broadcast）

飞牛 fnOS 上的一个小应用：**到点让小爱音箱说一句你写好的话**。

飞牛桌面点开就是一张播报表——周几、几点几分、说哪句话，各自开关；播报前可先把音量调到位；
失败自动重试；每次执行都留一条记录。不占端口，走飞牛统一网关。

![icon](packaging/fnos/xiaoai-broadcast/ICON_256.PNG)

## 它是怎么出声的

**不登录小米**，也不要求你在本应用里输密码。路径是：

1. 从 NAS 上 xiaomusic 的 `conf/.mi.token` 里取一份现成的小米票据（`passToken` / `micoapi` / `xiaomiio`）；
2. 优先走 **MiNA（小爱语音云）**：`POST https://api2.mina.mi.com/remote/ubus`，`method=text_to_speech`、`path=mibrain`，
   只认 cookie（`userId` + `micoapi` 的 serviceToken），不需要签名；
3. MiNA 不通时退回 **MIoT**：`POST https://api.io.mi.com/app/miotspec/action`，`siid=5, aiid=3`（Play Text），
   HMAC 签名，用的是 `xiaomiio` 的票。

这台音箱（小爱音箱 Pro / `L05C`）在 xiaomusic 的 `TTS_COMMAND` 表里对应 `5-3`，所以两条路说的是同一件事，
区别只在于：MIoT 这条对「客户端」限流较严（会回 `429 client withdraw`），MiNA 宽松得多。

票据会过期。过期时点一下界面上的「重新同步票据」重新读一次即可；xiaomusic 只要还在用，它的文件就是新的。

## 数据放在哪

| 东西 | 路径（NAS 上） |
|---|---|
| 程序本体 | `/vol1/@appcenter/xiaoai-broadcast`（`$TRIM_APPDEST`） |
| 配置与票据 | `/vol1/1000/docker/xiaoai-broadcast/config.json`（`0600`） |
| 播报历史 | `/vol1/1000/docker/xiaoai-broadcast/logs.jsonl` |
| 运行日志 | `$TRIM_PKGVAR/app.log`（`/usr/local/apps/@appdata/xiaoai-broadcast/`） |

配置特意放在用户卷上：飞牛重装同名应用会清 `$TRIM_PKGVAR`，放这里升级不丢。卸载时删掉这个目录即可。

## 安装 / 升级

```bash
# NAS 上
sudo appcenter-cli install-fpk /vol1/1000/下载/xiaoai-broadcast-<版本>.fpk
# 或者应用中心 → 手动安装
```

> 同名的应用**不能**用 `install-fpk` 直接覆盖（会报 `10236`）。升级要先 `app uninstall <app>` 再装；
> 配置在用户卷上，卸载重装不会丢。

## 第一次用

1. 打开「小爱播报」→「音箱」卡片点「同步票据」（首次用会自动尝试，失败才需要手动点）；
2. 下拉里选那台音箱；
3. 「试一句」确认能出声；
4. 回「播报表」改时间和文案 → 保存。

## 音乐

两条来源：

- **NAS 曲库**：应用自己起一个只读音源端口（默认 `18090`），把 `/vol3/1000/music`、`/vol2/1000/music`
  里的音频列成下拉；选中后地址形如 `http://<NAS局域网IP>:18090/m/<随机token>/<目录序号>/<相对路径>`。
  不依赖 xiaomusic 的 `/music` 路由，路径里带随机段，只读、只暴露音乐目录。
- **任意 http/https 地址**：填什么放什么。

播放走 MiNA 的 `player_play_music`（`music` 结构体 + `play_behavior: REPLACE_ALL`），失败退回
`player_play_url`——L05C 这类型号只认前者。

> 音箱必须和 NAS 在同一局域网；`18090` 被防火墙挡住时会「发送成功但没声」，用手机浏览器打开那个
> 地址就能确认。

## 故事（大模型）

「故事」卡片是两步：生成 → 看稿 → 「让小爱讲」才出声。生成走一个 OpenAI 兼容接口。

| 设置项 | 作用 |
|---|---|
| 接口地址 | 到 `/v1` 为止，程序自己拼 `/chat/completions` |
| 模型 | **用非推理模型**（如 `deepseek-chat`）；`*-reasoner` / `*-thinking` 会先思考再写，慢好几倍 |
| 输出上限 | 作为 `max_tokens` 发送，默认 800；调小更快，太小会截断 |
| 思考强度 | 作为 `reasoning_effort` 发送，默认不发送（部分厂商不认这个字段会回 400） |

> 实测：换成非推理模型（`deepseek-v4-flash-none`）后从几十秒降到几秒。生成结果里会显示「模型 · 字数 · 用时」，方便对比。

## 出问题

| 现象 | 处理 |
|---|---|
| 徽标显示「还没同步票据」 | xiaomusic 没登录/没跑，`conf/.mi.token` 不存在；让 xiaomusic 先登录成功，再点「同步票据」 |
| 「同步票据」报错 401/空 | 同上；票据文件过期了也会这样，重新导入 |
| 试一句没声、记录里有 `MiNA 通道失败` | 后面还会跟一条 MIoT 的结果，两条都失败才是真失败；把记录发我 |
| 记录里 `429 client withdraw` | 小米对 MIoT 接口限流，等几分钟；MiNA 通常不受影响 |
| 到点没响但记录成功 | 音量太小/免打扰：把「播报前把音量调到」设成 30–50，并确认音箱 App 里免打扰时段 |
| 换了音箱 | 「刷新设备」→ 下拉里改选 |

## 构建

```powershell
pwsh -NoProfile -File pack.ps1
# 产物：dist\xiaoai-broadcast-<version>.fpk
```

需要 Go（交叉编译 linux/amd64）与 GNU tar（Git for Windows 自带）。

本机不用 fnpack：它在这台机器上一 mkdir 就 `Access is denied`（安全软件拦未签名程序的写入）。
fpk 本身是 tar.gz（`app.tgz` + `manifest` + `cmd/` + `config/` + `ICON*.PNG`），`pack.ps1` 用 GNU tar
按同一结构打，`--mode=0755` 保证 `cmd/main` 可执行。

## 目录

```
main.go        HTTP 路由、调度循环、播报入口
mina.go        MiNA（小爱语音）：换票、设备列表、text_to_speech、播放/音量
xiaomi.go      小米云：签名、设备列表、MIoT 5-3、票据续期
import.go      从 xiaomusic 的 .mi.token 导入票据
music.go       曲库扫描 + 只读音源服务（/m/<token>/...）
story.go       大模型生成故事 + Key 掩码 + 连接测试
store.go       配置、执行记录、下次播报推算
web/index.html 单页界面（内嵌进二进制）
packaging/fnos/xiaoai-broadcast/   fpk 包目录（manifest、生命周期脚本、图标）
tools/mkicon.ps1                  图标生成（GDI+，四份尺寸）
pack.ps1                          编译 + 打包 + 自检
```

## 维护

- **升级不丢配置**：配置和票据在 `/vol1/1000/docker/xiaoai-broadcast/`，应用卸载重装都不会动它；
  只有 `$TRIM_PKGVAR`（生命周期日志）会随卸载清掉。
- **回滚**：旧版本 fpk 若还留着，重新装回即可；配置结构向后兼容（新增字段都有默认值）。
- **换机器/换端口**：`设置 → 曲库与音源` 里改目录与端口（端口改动重启应用生效），不用重新打包。
- **票据过期**（一般几个月）：`设置 → 票据 → 重新同步票据`，从 xiaomusic 的 `.mi.token` 再读一份。
- **代码仓库**：本工程是独立 git 仓库（`D:\ProgramData\projects\XiaoaiBroadcast`），
  `dist/` 与编译出的二进制不入库，打包用 `pack.ps1` 重现。
