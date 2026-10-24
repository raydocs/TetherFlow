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
