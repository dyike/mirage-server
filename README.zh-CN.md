# Mirage Server

[English](README.md) | [简体中文](README.zh-CN.md)

从 `mirage` 提交 `8ed35bd` 拆分。客户端位于同级 `mirage` 仓库。

内部协议和媒体包为 Mirage 自有源码的独立快照，包含客户端互操作测试所需实现；后续修改需按需同步两端。

### ARD 高性能模式（复用现有 Mac 客户端）

Go 服务端增加 `-ard -ard-udp-port 5902` 后，同时接受 Apple `RFB 003.889` 和标准 `RFB 003.008`。现有 Mirage 客户端无需更新代码：新建 **Mac** 类型连接，选择 **自动画质**，填写系统账户、密码和 `5902` 端口，即复用现有 HEVC/H.264 SRTP 接收与硬件解码路径。原有 **VNC** 类型连接继续使用 VeNCrypt/TCP。

- ARD 路径使用现有客户端支持的 security type 30、Apple 加密记录层、MediaStream 协商，以及 AES-256 SRTP/SRTCP。账户可由 `-pam-service` 验证；固定账户使用 `-username` 和 `-password-file`。ARD 的用户名和密码字段各最多 63 字节。标准 VNC 的 TLS 证书校验保持原样；不要把带有 VNC 证书指纹的旧配置直接改成 Mac 配置来绕过校验。
- 首个媒体会话使用 `5902/UDP`，后续并发会话依次使用后面的端口（默认最多四个，即 `5902–5905/UDP`）。UDP 端口在媒体协商时绑定；`-ard-udp-port 0` 使用临时端口，适合测试。外网需要相应的 UDP 放行及同端口转发，仅转发 TCP 不够。
- 控制、键鼠和文本剪贴板保留可靠传输；视频独立通过 SRTP。服务端根据认证后的 RTCP 丢包与排队反馈降低码率，按关键帧请求让现有编码器原地生成关键帧（不重建采集管线）。发送队列受限，完整帧不会因正常限速等待而被截断重启；单包丢失不直接触发码率骤降。原生 DMA-BUF 采集在画面静止时复用 GPU 帧保持媒体输出，避免客户端误判断流，不映射 CPU 像素。客户端在原端口重新协商低码率时，服务端接受新 SRTP 密钥并继续视频，不再拒绝重新协商。UDP 不通时，现有客户端在首帧等待超时后回退 TCP 画面。
- 已验证现有 Mirage Mac 客户端的 H.264/HEVC 硬件解码、DMA-BUF 零拷贝、丢包恢复及 UDP 黑洞回退。此实现针对 Mirage 已有的 ARD 子集，未宣称兼容 Apple 原生 Screen Sharing.app 的全部功能。
- ARD 通道支持系统输出音频：服务端通过 PipeWire 捕获默认输出监视器（不采集麦克风），使用系统 `libfdk-aac.so.2` 编码为 48 kHz 双声道 AAC-ELD，独立 SRTP 音频通道为 128 kbit/s。缺少编码库时仅关闭 ARD 音频并记录原因；`-no-audio` 可禁用。音频端口紧随视频端口池，默认四个会话为 `5906–5909/UDP`，VPN/防火墙也需允许这些端口。视频降码率重新协商时音频同步换密钥并重启。现有 Mirage 客户端无需更新，标准 VNC 原有音频不受影响。画面使用服务端配置的输出；码率自适应及重新协商始终保持输出分辨率，不因低码率或大帧自动缩小画面。大帧按协商码率完整发送，低带宽下可能增加延迟。输入坐标仍对应原生桌面。客户端手动缩放及多屏切换尚未实现。

硬件端到端验证（需要本机 VA-API、GStreamer 和 EGL）：

```bash
MIRAGE_TEST_RFB_GPU=1 go test -race -tags purego ./internal/server -run '^TestARDHardwareInterop$' -v
# 真实 4K 输出，模拟每 200 个视频包丢 1 个；仅输出计数与时序，不记录画面
MIRAGE_TEST_ARD_SOAK=1 MIRAGE_TEST_NATIVE_OUTPUT=MIRAGE MIRAGE_TEST_DROP_EVERY=200 go test -race -tags purego ./internal/server -run '^TestARDLocalSoak$' -v
```

### HEVC 扩展

Go 服务端支持私有编码 `0x4d485643`（MHVC），只有客户端主动声明才会使用，普通 VNC 客户端仍协商已有编码。当前实现为 VA-API HEVC Main 8-bit，禁用 B 帧；不是标准 RFB HEVC 扩展。

客户端运行 `MIRAGE_VNC_HEVC=1 go run -tags purego ./cmd/mirage`，选择 VNC 和自动画质，连接服务端地址的 `5902` 端口。HEVC 目前是实验开关，默认不启用。

### Go 服务端开发入口

服务端在本仓库独立开发，使用 Go Modules 管理依赖。入口为 `cmd/mirage-server`，代码位于 `internal/server/`。Linux 支持按输出名称直接使用 Wayland ext-image-copy-capture 采集，也可通过桌面 Portal 获取 PipeWire 流；使用 purego 调用 GStreamer/VA-API 编码 HEVC/H.264，不启动 WayVNC/NeatVNC 子进程。

捕获验证（弹出系统屏幕选择框，编码数据仅计数，不保存或发送）：

```sh
go run ./cmd/mirage-server -codec hevc -probe-frames 60
```

添加 `-test-source` 使用合成画面。原生库运行时需要 PipeWire、GStreamer VA、编码解析器与 appsink 插件。

加密 VNC 服务端（现有 Mirage 客户端选择 VNC 即可，不需要新协议）：

```sh
go run --tags purego ./cmd/mirage-server -listen 127.0.0.1:5910 \
  -cert server.crt -key server.key -password-file password.txt \
  -input-output MIRAGE -keyboard-layout us
```

`password.txt` 保存 1–8 字节 VNC 密码（末尾换行会去除）；VNC challenge/response 在 VeNCrypt X509Vnc 的 TLS 内传输。若指定 `-username`，则改用 TLS 内的 X509Plain 账户认证，支持最多 256 字节密码；客户端填写相同用户名即可。客户端沿用已有证书验证/确认流程。服务端使用 RFB 3.8，参考 [RFC 6143](https://www.rfc-editor.org/rfc/rfc6143.html)，不使用之前实验的 `MIRAGE/2` 协议。

编码由现有客户端协商：H.264、已启用的 Mirage HEVC 扩展、ZRLE、Tight、Raw 均可使用。清晰模式优先 ZRLE；Tight/ZRLE 使用连接内持续的 zlib 流。Raw 支持 8/16/32 位 true-color 像素格式和大小端。视频切换、尺寸变化或码率调整会重启编码器，从独立画面开始。连接从最高 2 Mbit/s 起步，默认上限 8 Mbit/s、30 FPS。视频编码使用 VBR、小 CPB；驱动在复杂画面上仍可能超过目标，因此发送端还按实际编码字节限速，允许最多 50 ms 突发，不丢弃有参考依赖的编码帧。TCP 发送队列和真实写入阻塞用于快速降码率，稳定后缓慢恢复，最大视频码率由 `-bitrate` 限制。编码前的有界队列丢弃过时原始帧；音频和协议开销不包含在视频码率限制内，48 kHz 双声道 S16 音频约占 1.536 Mbit/s。

功能与连接方式：

| 功能 | Go 服务端实现 |
| --- | --- |
| 认证 | RFB 3.8、VeNCrypt TLS、VNC 密码或账户密码；验证后才创建桌面控制设备 |
| 键鼠 | XKB 布局、组合键、物理键码、按键去重、LED、五键鼠标、四向滚轮、断线释放 |
| 输入扩展 | QEMU Extended Key Event、QEMU LED State、Extended Mouse Buttons；只在协商后启用 |
| 剪贴板 | Wayland data-control 双向文本；经典 Latin-1 和扩展 UTF-8 文本协议；长度与解压上限 |
| 音频 | PipeWire 输出音频，QEMU Audio 协商、启停、格式切换；默认 48 kHz 双声道 S16LE；不采集麦克风 |
| 光标 | 原生 Wayland cursor capture、本地 Cursor 形状与热点、随编码尺寸缩放；不支持时选择嵌入画面 |
| 多屏 | Portal 多流、VA-API 合成、ExtendedDesktopSize 布局、按 screen ID 路由输入、同一指针跨屏拖动 |
| 尺寸调整 | 可选连接内缩放/重排；可选原子 wlr-output-management 模式配置，拒绝时返回失败状态 |
| 传输 | 连续更新、Fence 回复、统一消息写锁、有限缓冲、慢连接超时、共享/独占连接语义 |

所有服务端代码位于本项目，依赖继续通过根 `go.mod` 管理。输入、剪贴板和光标由 Go 直接处理 Wayland 消息和 FD；系统 libxkbcommon、PipeWire、GStreamer/VA-API 通过现有 purego 依赖调用，不启动 C 服务端子进程。

`-output MIRAGE` 按 Wayland 输出名称直接采集整屏，无需 Portal、选择框或恢复令牌，同时将键鼠绑定到同一输出；不存在的输出直接报错。此路径要求 compositor 支持 `ext-image-copy-capture-v1`、`ext-output-image-capture-source-v1`，当前只接受单屏。H.264/HEVC 使用 GBM 分配的 DMA-BUF，经 `zwp_linux_dmabuf_v1` 交给 compositor 写入，再由 GStreamer/VA-API 导入；不经 CPU 映射或像素复制。六帧有界池在编码器释放最后一个内存引用后复用缓冲区，缩放和颜色转换在 GPU 上完成。编码路径禁止 CPU 映射，不支持 DMA 导入时明确报错，不自动降级。当前支持同一 DRM 设备的线性 XRGB/ARGB、未旋转输出；其他修饰符或旋转输出会报错。Raw/Tight/ZRLE 像素编码仍使用 `wl_shm` 的 CPU 路径。

`-output auto` 在启动时自动发现 Wayland 命名输出（最多 16 个），按名称排序，默认采集第一个。配合 `-ard`，Mirage 客户端已有的 Remote Displays 面板可以切换屏幕：每个连接独立选择，画面和鼠标映射使用相同的屏幕 ID，不修改物理屏模式。服务模板默认启用此模式；仍可通过 `-output DP-1` 或 `-output MIRAGE` 固定共享目标。

当前自动模式采用嵌入光标，编码尺寸由 `-width/-height` 决定；更新后的客户端显示实际输出名称，并标记默认屏幕；All Displays 按实际逻辑位置拼接全部屏幕，并在编码尺寸上限内保持桌面比例。多屏 H.264/HEVC 使用 DMA-BUF 采集、OpenGL 缩放合成，再在 GPU 内部 blit 到线性 GBM 缓冲交给 VA 编码；全程不进行 CPU 像素映射、回读或上传。采集池和合成目标池按下游释放引用后复用。GPU/EGL 不支持时明确报错，不静默降级为 CPU 合成；Raw 编码仍使用 CPU 路径。名称和单屏限制通过客户端主动请求的 Mirage 布局扩展传输，旧客户端仍显示屏幕编号。自动模式每 500ms 检查输出位置、逻辑尺寸和像素尺寸，连续两次一致后自动重建布局、采集与输入映射；缩放、分辨率变化或插拔屏幕会使现有客户端短暂断开并重连，无需手动重启服务。停用或没有有效逻辑尺寸的输出不进入可选列表；所有屏幕不可用时服务等待输出恢复。此功能尚不提供屏幕类型识别。自动发现会使所有发现的输出对已认证客户端可选。

不传 `-output` 时仍使用 Portal。`-input-output` 必须与 Portal 中所选屏幕对应，省略时不开放键鼠。多屏使用 `-multiple`，并按 Portal 流的顺序填写逗号分隔的输出名称，例如 `-input-output DP-1,HDMI-A-1`；Portal 后端必须支持多选。多屏坐标使用 XDG logical geometry，处理不同桌面缩放；键鼠共用一个虚拟设备，跨屏不重新按下鼠标键。

`-input-seat` 选择 seat；`-keyboard-rules/model/layout/variant/options` 配置 XKB；`-input-transform` 指定捕获图像的 Wayland 变换（0..7）。`-no-clipboard` 和 `-no-audio` 可分别关闭剪贴板和桌面音频。`-max-clients` 默认 4，每个连接独立维护认证、编码、输入和音频状态。

`-cursor-mode auto` 在提供输出名称且 compositor 支持 cursor capture 时选择本地光标，否则嵌入画面；也可显式选择 `local` 或 `embedded`。本地模式面向支持标准 Cursor 编码的客户端，现有 Mirage 已支持。

`-allow-resize` 允许客户端通过标准 SetDesktopSize 请求调整编码尺寸、选择已授权的屏幕或重排布局，不改物理显示模式。额外开启 `-resize-output-mode` 才会向 compositor 提交物理输出模式和位置修改；其他输出保持原状。物理配置是否成功由 compositor 决定，代码已验证成功/拒绝的协议路径，未在本机修改真实显示模式。

客户端保持兼容，但服务端不能增加旧客户端尚未发出的协议请求：当前 Mirage VNC 剪贴板发送的是经典 Latin-1，切屏/服务端缩放按钮仅针对 Mac 协议；Go 服务端的 UTF-8 扩展剪贴板、标准 VNC 布局修改和物理键码扩展供已实现对应协商的客户端使用。普通画面、已有键鼠、音频和光标无需改客户端。

验证覆盖现有客户端的认证、三种像素编码、持续解压、键鼠、双向文本、音频与光标；另有扩展 UTF-8、QEMU 输入、连接内缩放、并发客户端和异常清理测试。真实机器验证了 VA-API H.264/HEVC 硬解零 CPU 回读、双流合成、PipeWire 输出音频和 Hyprland 光标。多屏真实显示器热插拔、各 compositor 的 Portal 行为和长时间多客户端压力仍需部署环境验证，不能据此宣称与 WayVNC 所有平台行为完全等价。

运行测试：

```sh
go test -race -tags purego ./cmd/mirage-server ./internal/server/... ./internal/rfb
go vet -tags purego ./cmd/mirage-server ./internal/server/...
# 合成画面和 GPU 编解码（不捕获桌面内容）
MIRAGE_TEST_RFB_GPU=1 go test -tags purego ./internal/server/... -run 'TestRFBHardwareInterop|TestCompositeCapture' -v
# 真实 DMA-BUF 采集、禁止 CPU 映射、缓冲区释放与客户端硬解联调
MIRAGE_TEST_NATIVE_OUTPUT=MIRAGE go test -race -tags purego ./internal/server/input ./internal/server/capture -run 'TestNativeDMA|TestNativeOutputCapture|TestDMAOwnership' -v
MIRAGE_TEST_RFB_GPU=1 MIRAGE_TEST_NATIVE_OUTPUT=MIRAGE go test -tags purego ./internal/server -run TestRFBHardwareInterop -v
# 真实输出音频、光标、剪贴板连接；不发送键鼠或改写桌面剪贴板
MIRAGE_TEST_NATIVE_OUTPUT=MIRAGE go test -tags purego ./internal/server -run TestNativeServicesWithExistingClient -v
```

构建并安装 Go 服务端（默认安装到 `~/.local/bin`，不启动或重启服务）：

```sh
bash scripts/build-mirage-server.sh
# 或指定绝对安装前缀
bash scripts/build-mirage-server.sh /绝对安装前缀
```

本机 5902 的 Go 服务模板为 `scripts/mirage-server.service`，安装到 `~/.config/systemd/user/` 后使用 `systemctl --user enable --now mirage-server.service` 启动；先停止占用同一端口的旧服务。二进制安装在 `~/.local/bin/mirage-server`，部署前按本机调整输出名称、尺寸和证书路径。5900 的服务独立运行。

`-pam-service system-auth` 使用 `/etc/pam.d/system-auth` 的系统 PAM 账户认证策略，代替 `-password-file`；其他发行版需选择对应的 PAM 策略。证书和私钥分别放在 `~/.config/mirage-server/tls_cert.pem`、`~/.config/mirage-server/tls_key.pem`，私钥权限设为 0600。迁移已有服务时复制原证书和私钥，可保持客户端已保存的证书指纹；客户端仍填写系统账户和密码。模板使用 `-output auto`，首次启动和重启均直接选择指定整屏，不弹框。使用 Portal 模式时，首次选择整屏；`-restore-token-file` 以 0600 权限保存共享恢复令牌，后端支持时重启无需重新选择。令牌属于本机私密状态，不应提交到仓库；后端拒绝恢复时仍需重新选择屏幕。

### Mac 原生屏幕共享的 SRP 认证

`-ard -srp-file /path/to/srp.json` 启用 Apple Type 36 SRP-6a，严格禁止回退到旧式 Type 30。使用 RFC 5054 的 4096 位组、SHA-512、PBKDF2-HMAC-SHA512 和常量时间模运算。客户端校验服务器的 M2 证明，服务端校验客户端 M1；会话密钥继续保护已有 Apple record layer 和媒体密钥交换。这里的服务器认证基于 SRP 密码凭据，不是 CA 证书、RSA 主机指纹或 Kerberos；未实现 Type 33/35。

先在服务器本机终端登记（输入隐藏，不通过命令行参数或聊天传递密码）：

```sh
mirage-server -enroll-srp -username "$(id -un)"
```

默认保存到 `$XDG_CONFIG_HOME/mirage-server/srp.json`（未设置时为 `~/.config/mirage-server/srp.json`），文件权限 0600，原子替换，只保存盐、迭代次数和校验值。可用 `-srp-file` 指定位置。一个文件对应一个账户；这是独立的 Mirage 认证凭据，可以设置为与系统密码相同，但不自动随 PAM 密码变化，也不执行 PAM 账户有效期策略。修改或撤销时重新登记或删除文件，已有会话需重启服务才能立即撤销。

服务启动参数加入 `-srp-file %h/.config/mirage-server/srp.json` 并移除 `-pam-service`。凭据每次连接读取，登记后无需重启；缺失、损坏或权限过宽时拒绝登录，不降级。凭据必须通过本机可信终端登记，不能从旧式远程登录自动学习。Apple Type 36 的真实 macOS 互操作需在目标版本上测试；自动测试覆盖 Mirage 客户端的双向证明、加密画面、错误密码、伪造服务器证明、无效公钥、异常长度与降级拒绝。

SRP 凭据格式 v2 固定使用经过 RFC 5054 摘要校验的 4096 位组。早期 v1 的组常量缺少一个十六进制字符，Mac 原生客户端会在挑战阶段拒绝；该版本凭据不能转换，升级后必须在本机重新运行一次 `mirage-server -enroll-srp`，可以使用相同密码。

## 开源协议

Mirage Server 使用 [MIT License](LICENSE) 开源。第三方依赖及随附组件保留各自的许可证和声明。

## 远程音频独占

启用 `-audio-exclusive` 后，仅在远程音频工作期间将当前用户的 PipeWire/PulseAudio 播放流切到静音虚拟输出，由 Mirage 定向采集并传给远端；不会修改实体设备的音量和静音状态。ARD 收到认证通过的音频接收反馈后才接管声音，仅视频连接不接管。

记录原默认设备及已有应用音频去向，独占期间新音频流也会被接管。最后一个音频流结束后等待一秒再恢复本机，避免 SRTP 短暂重新协商导致扬声器突然出声。ARD 音频反馈超过三秒未到达时停止采集并释放接管；音频传输超时后需重新连接。

需要 `pactl` 和 PipeWire 的 PulseAudio 兼容服务。影响当前用户的桌面音频，而非仅远程画面中的应用；绕过 PipeWire/PulseAudio 直接使用 ALSA 的应用不在范围内。多个远端可共享音频，但同一用户只能运行一个负责独占路由的服务进程。

恢复状态以私密文件保存于 `$XDG_RUNTIME_DIR/mirage-audio/`。正常退出会恢复音频，下次启动也会恢复异常退出遗留的状态。systemd 服务应在启用该参数时额外添加 `ExecStopPost=%h/.local/bin/mirage-server -restore-audio`，用于进程崩溃后的恢复；也可在服务退出后手动运行 `mirage-server -restore-audio`。恢复时不覆盖用户已手动切换的默认输出或已离开虚拟输出的音频流。原设备不可用时选择其他可用输出；没有可用输出则保留恢复状态。
