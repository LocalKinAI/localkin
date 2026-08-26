# `kinclaw-mac` — Spotlight Shell 计划

**日期**: 2026-05-03 (v2 重写,先前版本撤回)
**状态**: 计划阶段
**仓库 (新)**: `kinclaw-mac` (待创建,与 `localkin-ios` 平级)
**产品名**: KinClaw Mac
**起点**: Fork [`localkin-ios`](https://github.com/LocalKinAI/localkin-ios) (1,756 行 Swift,生产可用)
**预期版本**: kinclaw-mac v0.1.0
**关联**:
- [`CHANGELOG.md`](../CHANGELOG.md) v1.8.0 承诺过的 Swift WKWebView shell
- [`localkin-ios`](https://github.com/LocalKinAI/localkin-ios) 复用源
- [`docs/roadmap.md`](roadmap.md) 老 v1.5 "Wails Console" 计划由本计划取代

---

## 一句话

**KinClaw Mac** = `localkin-ios` 的桌面孪生 + Spotlight 形态 + 双 endpoint (本地 KinClaw + 云端 160+ agents)。⌘⌥K 召唤,选任一 agent,操作你的 Mac 或聊任何专家。

---

## 形态 = 桌面端的"AI Dock"

> "**Your AI dock. ⌘⌥K to summon any of 160+ agents — or KinClaw to operate your Mac.**"

不是单纯 chat client,也不是单纯 computer-use 浮窗,**两件事的合体**:

| 选 agent | 你得到 |
|---|---|
| 🦞 **KinClaw** (本地) | 操作你真实 Mac (5 爪 + skills) |
| 📜 Selah | 神学讨论 (cloud) |
| 🌿 岐黄 | 中医问诊 (cloud) |
| 🔬 各 19 domain × 160+ agents | 任何专家咨询 (cloud) |

⌘⌥K 召唤同一个浮窗,内部切 agent。

---

## 为什么走 fork 路线

### 已有资产 (localkin-ios) — 1,756 行 Swift,直接复用

```
LocalKin/
├─ APIClient.swift          106 行  ✅ REST + auth (云端 agent)
├─ SSEClient.swift           99 行  ✅ 流式聊天
├─ VoiceRecorder.swift      239 行  ✅ 麦克风 + Whisper STT
├─ SpeechSynthesizer.swift  150 行  ✅ TTS (remote + local fallback)
├─ ChatHistory.swift         51 行  ✅ 每 agent 持久化
├─ TokenManager.swift        61 行  ✅ 认证
├─ LocalKinApp.swift        155 行  ✅ 入口 + AppState
├─ AgentListView.swift      443 行  ✅ 浏览 agents (= souls)
└─ ChatView.swift           452 行  ✅ 完整聊天 UI + voice 模式 + 流式
```

### 需要新写 — 约 340 行

| Mac 专属层 | LOC | 说明 |
|---|---|---|
| `KinClawShellApp.swift` (改 entry) | ~30 | LSUIElement、@main、AppDelegate |
| `SpotlightWindow.swift` (NSPanel) | ~80 | frameless 浮窗 + always-on-top |
| `KinClawSupervisor.swift` (子进程) | ~120 | 起 kinclaw serve + 健康监控 + 自动重启 |
| `HotkeyManager.swift` (全局) | ~30 | KeyboardShortcuts 包 |
| `MenuBarController.swift` (NSStatusItem) | ~50 | 🦞 menubar + soul switcher |
| `KinClawAPIClient.swift` (本地 endpoint adapter) | ~40 | `/api/chat` + `/api/events` (vs `/v1/chat`) |
| **总计新写** | **~340** | |

### 跨平台 SwiftUI 共享层 (改改就跑)

iOS↔macOS SwiftUI 大部分通用,以下需要 conditional compilation `#if os(macOS)`:

- `ChatView` — 99% 通用,touch gesture 改 click,键盘焦点处理略调
- `AgentListView` — 99% 通用,`UIScreen` 改 `NSScreen`
- `VoiceRecorder` — `AVAudioRecorder` 跨平台,但麦克风权限 prompt 不同 (iOS = `NSMicrophoneUsageDescription`,mac 同名但触发流程不同)
- `SpeechSynthesizer` — 完全通用 (`AVSpeechSynthesizer`)
- `APIClient` / `SSEClient` — 完全通用 (`URLSession`)
- `ChatHistory` — 完全通用 (`UserDefaults`)

### 时间线对比

|  | 之前估算 (从零写) | fork localkin-ios |
|---|---|---|
| Spike + 跑通 | 1 天 | **0.5 天** (deployment target 切换) |
| API 适配 | 0 (从零写就在写) | **1 天** (kinclaw `/api/chat` ≠ localkin `/v1/chat`) |
| Mac 专属层 | 5 天 | **2-3 天** (聚焦新 340 行) |
| Polish + 嵌 binary | 1 天 | **0.5 天** |
| **总开发** | **10 天** | **4 天** |
| 等 cert + 签名 | 2 天 | 2 天 |
| **总计** | **12 天** | **6 天** |

**砍一半**。

---

## 双 Endpoint 架构

```
┌──────────────────────────────────────────┐
│  KinClaw Mac (kinclaw-mac.app)           │
│                                          │
│  AgentSource selector:                   │
│   ┌─────────────────────────────────┐    │
│   │ ◉ Local (KinClaw)               │    │
│   │   • Pilot soul (5 claws)        │    │
│   │   • Other souls in ./souls/     │    │
│   │                                 │    │
│   │ ○ Cloud (api.localkin.dev)      │    │
│   │   • 19 domains × 160+ agents    │    │
│   │   • Auth via TokenManager       │    │
│   └─────────────────────────────────┘    │
│                                          │
│  Same ChatView, same voice, same UX      │
└──────────────────────────────────────────┘
              │             │
              ▼             ▼
   localhost:8020      api.localkin.dev/v1
   (kinclaw serve)     (160+ cloud agents)
```

**协议适配层** (`AgentSource` 协议):

```swift
protocol AgentSource {
    var name: String { get }
    var availableAgents: [Agent] { get async }
    func sendMessage(_ msg: String, to agent: Agent) -> AsyncStream<Token>
    func voiceTranscribe(_ audio: Data) async throws -> String
    func voiceTTS(_ text: String) async throws -> Data
}

class LocalKinClawSource: AgentSource { ... }  // 本地 kinclaw serve
class CloudLocalKinSource: AgentSource { ... }  // api.localkin.dev (复用 localkin-ios 已有代码)
```

ChatView 不关心来源,只看 protocol。

---

## API 适配 — kinclaw vs localkin

两边 API 不一样,需要桥接:

| | localkin (cloud) | kinclaw (本地) |
|---|---|---|
| 聊天端点 | POST `/v1/chat` (SSE 内联返回) | POST `/api/chat` + GET `/api/events` (分离) |
| 消息格式 | `{messages: [...], stream: true}` | `{message: "..."}` (单条 + history 服务端管) |
| 认证 | Bearer token | 无 (localhost) |
| Soul 切换 | per-request `agent_id` | POST `/api/soul {path}` (有状态) |
| 取消 | 关闭 SSE 连接 | DELETE `/api/chat` |
| Voice STT | POST `/v1/voice/transcribe` | POST `/api/voice/transcribe` |
| Voice TTS | POST `/v1/voice/tts` | POST `/api/voice/tts` |

复用 localkin-ios 的 `SSEClient` 思路,新写一个 `KinClawAPIClient` 处理 kinclaw 的两通道模式。

---

## 7 个里程碑 (4-6 天)

| M | 工作 | 工时 | 产出 |
|---|---|---|---|
| **M0 — Fork + Build** | clone localkin-ios → kinclaw-mac;改 project.yml deployment 为 macOS 13+;Xcode 编译过 | 0.5 天 | mac 上能跑 (此时只能聊云端) |
| **M1 — KinClawAPIClient** | 写本地 endpoint client;在 AgentSource 协议里加 LocalKinClawSource | 1 天 | 能聊本地 kinclaw |
| **M2 — Spotlight Window** | NSPanel + nonactivating + glass blur;ContentView 嵌进去 | 1 天 | 浮窗形态,但要从 menubar 触发 |
| **M3 — Hotkey + Menubar** | ⌘⌥K 全局触发 + 🦞 NSStatusItem | 1 天 | Spotlight 完整体验 |
| **M4 — Supervisor** | 启动 kinclaw 子进程 + 健康监控 + 自动重启 | 1 天 | 双击 .app 自包含 |
| **M5 — Polish** | 动画、位置记忆、尺寸记忆、快捷键自定义 settings | 0.5 天 | 像产品 |
| **M6 — Sign + DMG** | ⚠️ 等 $99 cert | codesign + notarize + create-dmg + R2 | 2 天 |

**总: 5 天开发 + 2 天签名分发**

---

## 关键技术决策 (基于复用 localkin-ios)

### 复用决定

- ✅ **复用 SwiftUI 架构** — 不切 AppKit,SwiftUI 在 macOS 13+ 很成熟
- ✅ **复用 SSEClient** (改改 endpoint 就行)
- ✅ **复用 ChatView/AgentListView** — 99% 通用
- ✅ **复用 TokenManager** — 云端 agent 鉴权同样需要
- ✅ **复用 ChatHistory** — UserDefaults 持久化跨平台

### 新决定

- 🆕 **NSPanel 而非 SwiftUI Window** — 浮窗 nonactivating 模式 SwiftUI 还做不干净
- 🆕 **MenuBarExtra (SwiftUI macOS 13+)** — 现代 API,跟 SwiftUI 风格一致
- 🆕 **deployment target macOS 13+** (跟 iOS 17 同代)
- 🆕 **LSUIElement = true** — 不在 dock 显示
- 🆕 **[KeyboardShortcuts](https://github.com/sindresorhus/KeyboardShortcuts) 包** — 全局快捷键

---

## 默认配置 (基于先前讨论 + 用户决定)

| | 默认 | 备注 |
|---|---|---|
| 名字 | **KinClaw Mac** (repo: `kinclaw-mac`) | 用户拍板 |
| Hotkey | ⌘⌥K | 避开 Spotlight ⌘Space / Alfred ⌥Space / Raycast |
| 启动模式 | Login Item (默认开启,Settings 可关) | always-available 形态需要 |
| 浮窗大小 | 用户可拉伸,记忆尺寸 | 默认 380×600 |
| 浮窗位置 | 记住上次拖动位置 | 默认中心 |
| 安全 | 锁死 localhost + api.localkin.dev | WKConfiguration 限制 |
| 多窗口 | MVP 单窗;v0.3 考虑 swarm | |

---

## 仓库结构 (新建 `kinclaw-mac`)

跟 `localkin-ios` 平级,在 `Workspace/` 下:

```
~/Documents/Workspace/kinclaw-mac/
├── README.md
├── CHANGELOG.md
├── LICENSE (Apache-2.0,跟 kinclaw 一致)
├── project.yml          # XcodeGen
├── KinClawMac.xcodeproj # 生成
└── KinClawMac/
    ├── KinClawMacApp.swift
    ├── Info.plist
    ├── Models/          # 从 localkin-ios 复制 + 加 KinClawSoul
    ├── Services/
    │   ├── APIClient.swift          # 复用 (云)
    │   ├── KinClawAPIClient.swift   # 新写 (本地)
    │   ├── AgentSource.swift        # 新协议
    │   ├── KinClawSupervisor.swift  # 新写
    │   ├── SSEClient.swift          # 复用
    │   ├── ChatHistory.swift        # 复用
    │   ├── TokenManager.swift       # 复用
    │   ├── VoiceRecorder.swift      # 微调 mac
    │   └── SpeechSynthesizer.swift  # 复用
    ├── UI/
    │   ├── SpotlightWindow.swift    # 新写
    │   ├── HotkeyManager.swift      # 新写
    │   ├── MenuBarController.swift  # 新写
    │   ├── ChatView.swift           # 复用 (mac 适配)
    │   ├── AgentListView.swift      # 复用 (mac 适配)
    │   └── SettingsView.swift       # 微调
    └── Resources/
        └── kinclaw                  # 嵌入的 Go 二进制 (M4)
```

---

## 30 视频 × kinclaw-mac

新产品形态让视频选题大大扩展:

**老选题** (只有本地 KinClaw):
1. ⌘⌥K 召唤 → KinClaw 帮我整理桌面
2. 朋友找房子 (browser_session)
3. KinClaw vs TuriX 安装对比

**新可能选题** (云端 + 本地组合):
4. ⌘⌥K → 切到 Selah → 神学问题 → 切到 KinClaw → 把答案存到 Notes
5. ⌘⌥K → 岐黄问诊 → KinClaw 自动在 Apple Health 记录
6. ⌘⌥K → 投资 agent 给意见 → KinClaw 自动下单 (... 慎)
7. 单一 hotkey,160+ 专家 + 你的 Mac 操作员,全在那

**这个组合是 TuriX 给不了的** — 他们只有 CUA,没有 cloud agent 网络。

---

## 风险

| 风险 | 概率 | 缓解 |
|---|---|---|
| iOS↔mac SwiftUI 兼容性踩坑 | 中 | M0 spike 0.5 天就能暴露;最坏 fallback AppKit |
| API 双源切换体验混乱 | 中 | UI 设计上 agent 列表区分明显 (本地 = 🦞 标记) |
| KinClaw 子进程权限 (麦克风、AX) | 中 | .app 第一次启动逐项 prompt |
| api.localkin.dev 鉴权流程要求联网 | 低 | 离线时 fallback 只显示本地 agents |
| WKWebView 跟 SSE | 极低 | localkin-ios 已经验证过 |
| 等 cert | 高 (外部) | M0-M5 不阻塞,等到了再 M6 |

---

## Out of Scope

- ❌ Windows / Linux 版本 — kinclaw-mac mac 专属;Windows 出来后另开 kinclaw-windows
- ❌ Multi-soul swarm UI — v0.3+
- ❌ 自动更新 (Sparkle) — v0.2+
- ❌ App Store 上架 — DMG 直接分发就行,App Store 沙箱限制太多 (没法操作 Mac)

---

## 一句话总结

**Fork localkin-ios 改成 macOS,加 NSPanel + 全局 hotkey + 菜单栏 + KinClaw 子进程管家 = 5 天产出 KinClaw Mac。形态是 Spotlight 浮窗 + 双 endpoint (本地 KinClaw 操作 mac + 云端 160 agents 聊天) = LocalKin 家族在桌面的真旗舰。$99 cert 卡 distribution,5 天开发先做。**
