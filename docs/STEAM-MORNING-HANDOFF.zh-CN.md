# 早上先看这里：TetherFlow iPhone + Steam 双网版

## 已经做出来的部分

- 独立 Windows 入口，不使用旧 PAC 假定 Steam 已被加速。
- 家庭 Wi-Fi 与 iPhone USB 蜂窝出口按连接/下载请求分配。
- HTTP 流式转发；HTTPS 原样透传，Steam 仍验证原站证书，不安装根证书。
- 内置 USBmux，不必默认再下载 iproxy；仍需要 Apple Devices 的苹果 USB 服务。
- 手机前台运行、每次启动换认证 token；Windows 在认证预检通过前不改 hosts。
- 本机端口限定、CDN 白名单、公网目标校验、32 手机连接预算、故障冷却/家庭回退。
- 两个出口的字节/区间速率、无请求明确提示、退出/崩溃清理与手动恢复。

## 不能冒充完成的部分

**还没有在你的 iPhone + Windows 上证明 Steam 接入或速度接近 1200 Mbps。**
目前测试中的 USBmux 服务、蜂窝出口替代物是受控本机 fixture，不是实体硬件。
Windows CI 证明 Windows 代码可运行，但其机器没有你的 Apple Devices/iPhone。

iOS App 还需要你的 Apple 开发签名和实际安装；无签名构建产物不能当成可安装 App。
当前机器有 Apple Development 签名身份，但没有选择团队、上传签名材料或擅自部署。
真实 iOS 27 支持、USB 协商/吞吐、实际运营商出口、Steam CDN 和磁盘瓶颈仍需确认。
不保证热点计费绕过，也不保证每个下载任务两路速度线性相加。

## 尽量压缩成一次设置、一次集中验收

1. **先安装手机端**：参照 `ios/README.md`，用支持你的 iOS 版本的 Xcode 打开生成项目，选择自己的 Team，在手机上签名运行。不要把证书私钥或账号密码发到聊天。
2. **Windows 一次设置**：Apple Devices 已安装并信任手机后，解压 Windows 包。先在手机点 Start；管理员 PowerShell 运行 `Start-SteamAccelerator.ps1`，选择家庭 Wi-Fi 本机 IP，隐藏输入手机 session token。无需设置整个系统的代理。
3. **集中验证**：先用脚本 `-CheckPhoneOnly` 检查128MiB内容完整的 USB 应用链路；再启动下载模式，重开 Steam 一次。看 Steam content_log 是否采用本地服务器、两路是否有流量，以及同一游戏下载的稳定速率是否比单路高。失败时看日志和自动状态文件，不反复换不相关设置。

完整操作、回退及故障判断：`desktop/steam-tools/README.zh-CN.md`。
程序默认仅监听本机家庭 IPv4 的80/443，拒绝邻居访问；如端口被占用会失败，不会关闭安全软件。

## 开发验证中实际发现并修正的问题

- 旧 PAC “双网”不等于真正 Steam 接管。
- guardian 启动竞态：改为启动前继承父进程句柄，Windows真实子进程测试覆盖快速退出。
- 总并发超过手机会话预算：改为覆盖拨号/活跃/空闲连接的总预算，容量不足不误判整条手机链路断网。
- Swift 偶发丢最后17字节：已复现为上游失败状态先于缓冲数据交付；旧源代码受控测试5/5失败，修复后50轮250项压力通过；不是靠忽略失败重跑。
- 长 TLS 下载不结束就显示0流量：现在边转发边累计。

本机已验证：19 Swift测试、5 Python测试、新 Go 包 race/vet/Windows交叉编译，跨语言全链路 HTTP + 证书验证 TLS 两模式重复10轮通过。
最终提交的 CI 与独立审查状态以本线程后续报告为准，不能从这份静态清单推断全部绿灯。
Devin 云验证申请被配额403拒绝，没有把缺失的模拟器/UI证据算成通过。
