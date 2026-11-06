# 早上先看这里：TetherFlow iPhone + Steam 双网版

## 当前状态（2026-11-06，先读这一段）

- **独立审查第 1 轮（R1）结论是 block，尚未解除；最新 CI 结果待定。** 其中“跨语言传输偶发 EOF”的历史根因**未知**（本机只在极端负载下复现 origin 自身 `ENOBUFS`/`EPIPE`，只是证据，不能证明同因）；新一轮审查须覆盖全部改动后才能交付。
- 已记录的 CI：42203841177（0cccceb）全绿；42207422505 在 `TestTLSHomeDNSFailureUsesPhone` 失败，其后的 Windows 健康修复尚无 CI 结论。更早的绿灯不外推到当前提交。
- R14 的 Windows 端到端/启动器/打包脚本**尚未在 GitHub Windows 上执行**；Windows 上 TLS 健康判断（客户端已 FIN + 已有一些下行字节后，上游错误按正常收尾处理）刚改，是启发式，不是整个 HTTP/TLS 响应完整的证明。
- **真机硬件关口 H1–H7 全部未验证**（签名安装、Apple Devices、蜂窝/DNS、token 手感、Steam 采用、三种模式基线、Windows 安全软件/断电）。表见 `docs/STEAM-RELIABILITY-PLAN.md` §5。
- 不声称 1200 Mbps、任何提速、Steam 一定采用此路径，也不声称修复了苹果原生以太网驱动。USBmux 不要求 iPhone 以太网适配器/网卡出现，但仍依赖 Apple Devices 的 Apple USB 服务栈。

## 已经做出来的部分

- 独立 Windows 入口，不使用旧 PAC 假定 Steam 已被加速。
- 家庭 Wi-Fi 与 iPhone USB 蜂窝出口按连接/下载请求分配；三种模式 `Dual`/`HomeOnly`/`PhoneOnly`，用于三份基线。
- HTTP 流式转发；HTTPS 原样透传，Steam 仍验证原站证书，不安装根证书。
- 内置 USBmux，不必默认再下载 iproxy；同一台手机重新插拔（新 DeviceID）按 serial 重连。
- token：手机端仅内存，暂停最多 10 分钟内回前台沿用同一个；过期/Stop/Rotate 后换新。Windows 只在程序自己的隐藏 stdin 里输入，**不落盘、不上命令行**；运行中 407 会提示重新输入（显式刷新，仍不回退到 Wi-Fi）。
- 每路独立 DNS：手机路径由手机解析并校验实际连上的公网对端；家庭路径用 PC 解析且全部答案须为公网。
- Windows 在认证预检（协议 2、USB 回环、手机 CONNECT、TLS 探测）通过前不改 hosts；hosts 写入有 write-ahead 日志，异常时保留日志并要求人工恢复，不覆盖别人的修改。
- 本机端口限定、CDN 白名单、32 手机连接预算、故障冷却/家庭回退。
- 报告区分“同窗口并发”与“活跃窗口速率”：`concurrent_seconds`、`active_seconds`、`sustained_mbps`（**不是整段会话平均**，低于 1 Mbps 窗口不计）。

## 不能冒充完成的部分

**还没有在你的 iPhone + Windows 上证明 Steam 接入或速度接近 1200 Mbps。**
目前测试中的 USBmux 服务、蜂窝出口替代物是受控本机 fixture，不是实体硬件。
Windows CI 证明 Windows 代码可运行，但其机器没有你的 Apple Devices/iPhone。

iOS App 还需要你的 Apple 开发签名和实际安装（`ios/tools/build-signed.sh`，只由你在自己的 Mac 上运行）；无签名构建产物不能当成可安装 App。
当前机器有 Apple Development 签名身份，但没有选择团队、上传签名材料或擅自部署。
真实 iOS 27 支持、USB 协商/吞吐、实际运营商出口、Steam CDN 和磁盘瓶颈仍需确认。
不保证热点计费绕过，也不保证每个下载任务两路速度线性相加。

## 尽量压缩成一次设置、一次集中验收

1. **先安装手机端**：参照 `ios/README.md`，用支持你的 iOS 版本的 Xcode，选择自己的 Team，在手机上签名运行（H1）。不要把证书私钥或账号密码发到聊天。
2. **Windows 一次设置**：Apple Devices 已安装并信任手机后，解压 Windows 包（核对 MANIFEST 的 SHA-256；未签名，SmartScreen 可能拦截）。先在手机点 Start；管理员 PowerShell 运行 `Start-SteamAccelerator.ps1`，选择家庭 Wi-Fi 本机 IP，在程序的隐藏提示里输入手机上显示的 32 字符 token（不是命令行、不是文件）。无需设置整个系统的代理。
3. **集中验证**：先用脚本 `-CheckPhoneOnly` 检查128MiB内容完整的 USB 应用链路；再依次用 `-Mode HomeOnly`、`PhoneOnly`、`Dual` 下载同一游戏，重开 Steam 一次。看 Steam content_log 是否采用本地服务器、两路是否有流量，比较各模式 `sustained_mbps` 与 `concurrent_seconds`（排除磁盘/解压阶段）。结果记录设备、OS、线缆、端口。不预设目标，记录实际结果。
4. 同时顺手验证 token：Control Center 下拉后回前台应无需重新输入；锁屏超过 10 分钟需要新 token 且旧的失败；负载下刷新。失败时看日志和自动状态文件，不反复换不相关设置。

完整操作、回退、hosts 日志/手动恢复及故障判断：`desktop/steam-tools/README.zh-CN.md`。
程序默认仅监听本机家庭 IPv4 的80/443，拒绝邻居访问；如端口被占用会失败，不会关闭安全软件。

## 开发验证中实际发现并修正的问题

- 旧 PAC “双网”不等于真正 Steam 接管。
- guardian 启动竞态：改为启动前继承父进程句柄，Windows真实子进程测试覆盖快速退出。
- 总并发超过手机会话预算：改为覆盖拨号/活跃/空闲连接的总预算，容量不足不误判整条手机链路断网。
- Swift 偶发丢最后17字节：已复现为上游失败状态先于缓冲数据交付；旧源代码受控测试5/5失败，修复后50轮250项压力通过；不是靠忽略失败重跑。
- 长 TLS 下载不结束就显示0流量：现在边转发边累计。
- 审查 R1 的发现已按顺序处理：刷新复用完整预检、恢复顺序（先修日志再检查 hosts）、verify-app 的 SIGPIPE 漏检、首读 EOF 误判；这些修复仍待新一轮审查复核。

## 本机验证记录（含历史）

较早的本机记录（历史，不代表当前提交）：19 Swift测试、5 Python测试、新 Go 包 race/vet/Windows交叉编译，跨语言全链路 HTTP + 证书验证 TLS 两模式重复10轮通过。可靠性改动后另有主线程 race 三轮和跨语言重复轮通过，细节见 `notes/steam-verification-final.md` 与 `docs/PROGRESS.md`。
旧的跨语言审查 EOF 阻断**未关闭**；最终提交的 CI 与独立审查状态以主线程后续报告为准，不能从这份静态清单推断全部绿灯。
Devin 云验证申请曾被配额403拒绝，没有把缺失的模拟器/UI证据算成通过。
