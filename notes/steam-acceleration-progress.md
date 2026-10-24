# Steam 双网加速：实施与验收

## 用户目标
Windows Steam 下载同时利用家庭 Wi-Fi（约 200–300 Mbps）与 iPhone 蜂窝（手机测速约 1200 Mbps）。USB 3 / 10 Gbps 数据线；可接受 iOS App 前台不锁屏和 Apple Devices 依赖。不能把 PAC 配置、编译成功或本机基准误报为真实 Steam 提速。

## 当前实施边界
- 原有 Android HighSpeedProxyEngine.kt 有用户未提交修改，禁止触碰。
- iOS 原型独立在 ios/：认证 USB 回环代理、强制蜂窝出口、背压与诊断。
- Windows 新核心独立在 desktop/internal/steamaccel/：请求级双出口调度、连接认证、流式转发、统计与回退。
- 计划独立启动器 desktop/cmd/steam-accelerator/，不自动启用老 PAC，不安装开机服务。

## 关键证据
- 现有 desktop/main.go 依赖 Android adb/8282；proxy_windows.go 仅设置系统代理/PAC。
- Lancache 上游确认 Steam 对 lancache.steamcontent.com 的发现和 Host 原样请求：https://lancache.net/news/2020/01/14/steam-client-now-supports-lancache/
- Steam 当前版本是否接受本机 RFC1918 IP、本机 hosts 覆盖及 HTTP 路径必须实机验证；127.0.0.1 不应默认假设可被识别。
- 本地分流不等于单连接绑定；Steam CDN、并发数、磁盘解压和蜂窝变化均影响结果。

## 必须保留的未验证项
签名安装 iPhone、Windows 真机接入、USB 协商与吞吐、Steam content_log 接入记录、双路同时字节增长、拔线回退、清理恢复、杀进程恢复、真实速度相对单路基准。

## 验收标准
1. 传输正确性、取消、错误重试、安全限制均有自动测试。
2. Mac host 测试/无签名 iOS 构建/Windows 交叉编译分开记录。
3. 实机测速前不得宣称 1200 Mbps 或双网提速已达标。
4. 不收集 Steam 凭据，不做 TLS 解密，不绕过正版校验。

## 2026-10-24 自动验证进展
- 本地主线程重跑：Go 三个新包 race tests 连续3次通过；go vet通过；Windows amd64交叉编译通过。
- 主线程重跑 Swift17测试、Python5测试通过。
- 已推送隔离 WIP 分支，未改 main、未发布、未创建 PR。独立 GPT xhigh worktree审查中。
- GitHub Windows/macOS真实托管 runner验证已启动（首个run40081992644）；不替代物理手机/Steam。
- Devin iOS云验证请求被403拒绝：organization billing error `out_of_quota`。未启动VM，不重复耗费重试；模拟器UI证据缺失，不在本机安装运行时。
- 加强guardian：父进程退出监控改用启动前继承的Windows进程句柄，消除“子进程还没OpenProcess父进程已退出”的窗口；对应Windows专用子进程测试已添加。
- GitHub首轮40081992644已确认：Windows真实执行race测试/Go vet/构建、PowerShell解析和只读doctor通过；macOS17项Swift和5项Python通过。此时commit=f1b86fe，后续修复需重跑。
- 新增真正跨语言集成（生产Swift核心+Go双出口，非模拟器）：初次有7次unexpected EOF（其余2轮通过），加fixture日志后3/3和20/20通过，未修改生产行为/期望值。初次失败根因未知，明确保留可靠性风险，并加入CI重复10轮。
