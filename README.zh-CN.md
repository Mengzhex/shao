[English](README.md) · **简体中文**

# tmon

录下你的终端,让 AI 助手回头去读——**只读,而且只在你提问时才读**。

它在本机把 shell 的字节流完整录下来,通过 MCP 提供给助手。数据只在你问了问题、助手因此调用工具的那一刻被读取一次。没有监控,没有推送,助手也执行不了任何东西。

助手看过去是这样:

```
4 session(s), newest first:

- id=20260828T152532.511-63068  live
    cwd=C:\proj\infra
    title="Administrator: pwsh"
    last=terraform plan -> exit 1
    shell=pwsh  buffered=3.2 MiB  rate=2.1MB/min  reaches_back=~1.4h  commands=17
- id=20260828T152532.441-76240  ended
    cwd=C:\proj\webapp
    last=npm run build -> ok
```

它解决两件事:

- **"刚才那个为什么失败了?"** 输出已经被 tmon 录下来了,助手可以直接看那条失败的命令和它一字不差的输出,不需要你从早已滚走的屏幕里翻出报错再粘贴一遍。
- **"这东西该怎么部署?"** tmon 能读取目标主机的实际情况——剩余磁盘、内存、哪些端口被占了、docker 和 nginx 现在在跑什么——所以建议是基于那台机器给的,而不是泛泛而谈。助手写命令,**你**来执行。

## 它和别的方案不一样在哪

已经有一些 MCP 服务器能给助手一个终端。**它们解决的是相反的问题**:给模型一个它自己的 shell,让它在里面执行命令。tmon 是让它读**你的**终端,而且它什么都执行不了。由此衍生出五点差别。

**不做任何采样,而且"完整"是可核查的。** 采集的是穿过 pty 的字节流,所以滚动速度快到你来不及看的输出也一字不漏地在那儿。更有用的是:每次读取都会分别告诉你环形缓冲是否已经丢弃过更旧的内容(`truncated`),以及是否发现了**真正的**不连续(`gap_detected`)。"什么都没丢"是一个**你可以去核实的断言**,而不是一句承诺。

**只读是 sshd 强制的,不是我们自觉。** 这个约束不是"我们没给 AI 注册执行工具"这种软的东西——它是挂在密钥上的 forced command,sshd 会**丢掉** SSH 客户端要求执行的任何内容。**即使这一侧被完全攻破,它依然成立。** 而 `tmon host verify` 是靠**真的去试**来证明它:执行一条命令、读 `/etc/passwd`、创建文件、申请终端、转发端口——五项全部被拒绝才算通过。

**不改变你的终端习惯。** 采集位于 pty 这一层,在终端模拟器**之下**,所以 Windows Terminal、iTerm、PuTTY、tmux、VS Code 的面板全都照常工作。`ssh` 会话是白送的:你在被录制的 shell 里 ssh 到远程服务器,那边看到的一切都录在**这边**,远端不装任何东西、不需要任何权限——这恰恰对那些你装不了东西的机器最有用。

**一个端点,多少个终端都行。** 压根不存在"按终端配置"这回事,所以也就没有会失去同步的东西。会话自带工作目录、标题和上一条命令,所以开十个终端也彼此可辨——你问的是 `cwd:webapp`,而不是一个还得先去翻出来的 id。

**你不问,就什么都不跑。** 没有定时器、没有监视、没有告警、没有后台任务。在你两次提问之间,tmon 只是一个往磁盘写东西的录制器,别的什么都不是——这也是为什么它没有什么需要"关掉"。

另外它是一个静态单文件二进制。没有运行时、没有要装的守护进程、被查询的服务器上也没有 agent。

## 它围绕的两条规则

**只在被问时拉取。** 不监控、不盯着、不推送。没有定时器,没有告警,没有后台任务。工具只在你提问、助手因此调用它的那一刻运行,其余任何时刻都不运行。

**AI 能读、不能执行——这由操作系统保证,而不是靠信任。** 对远程主机,tmon 持有的那把 SSH 密钥是以 *forced command* 的形式装到目标机上的:sshd 会丢掉 SSH 客户端要求执行的任何东西,改为运行一个固定的只读探针。探针只接受一份**封闭列表**里的动词,不接受任何自由形式的参数,所以整条链路上没有任何一个字符串能让指令钻进去。**即使这一侧完全被攻破,这个约束依然成立。** 无法按这种方式配置的主机会被标记为 `disabled`,根本无法查询——这里刻意没有留"tmon 保证自己会守规矩"这种弱化模式。

## 采集是怎么做的,以及为什么不用截图

tmon 把你的 shell 跑在一个伪终端(pty)里,录下穿过它的字节流。shell 写出的每一个字节都恰好经过那个管道一次,顺序不变。**没有任何采样**,所以滚动速度快到你根本来不及看的输出也被完整记录;十分钟后再来问,拿到的是完整画面,而不是"两次采样瞬间屏幕上刚好有什么"。

因为采集发生在**本机的 pty** 上,它不关心你用哪个终端模拟器——Windows Terminal、PuTTY、MobaXterm、iTerm、VS Code 的面板全都在它的外侧。它同样能录 `ssh` 会话:你在一个被录制的 shell 里 ssh 到服务器,在那边看到的一切都被录在**这边**,服务器上不需要装任何东西,也不需要任何权限。

每个会话保留两条流。**raw** 流是逐字节精确的。**cooked** 流应用了回车、退格和擦除序列并去掉了颜色码,所以一个重绘了 4000 次的进度条读起来就是它最终定格的那一行。助手默认读 cooked。

**唯一真实的限制:** 录制从 shell 启动的那一刻开始。**已经开着的终端窗口无法追溯录制**——任何无损方法都做不到这件事。所以 `tmon start` 一次覆盖两头:它录制你敲命令的这个终端,同时装一个启动钩子,让之后打开的每个终端自己就开始录。第一条命令之后,再没有任何"按终端"要做的事。

## 安装

tmon 是一个单文件二进制,没有运行时依赖。下载、放到 `PATH` 上,完事。

### 最快的方式

Linux 和 macOS——包括你刚 ssh 进去的服务器:

```sh
curl -fsSL https://github.com/Mengzhex/tmon/releases/latest/download/install.sh | sh
```

Windows,在 PowerShell 里:

```powershell
irm https://github.com/Mengzhex/tmon/releases/latest/download/install.ps1 | iex
```

两个脚本都会自动识别平台、用 `checksums.txt` 校验下载、装到 `~/.local/bin`
(以 root 运行则是 `/usr/local/bin`,Windows 上是 `~in`),并在该目录不在
`PATH` 上时提示你。用 `TMON_INSTALL_DIR` 改安装位置,用
`TMON_VERSION=v0.1.0` 锁定版本。

把网上的脚本管道进 shell 是一个**真实的决定,不是走形式**。这个脚本很短,也
没做任何花哨的事——[先读一遍](scripts/install.sh)也完全合理,或者直接用下面
的手动步骤,脚本做的就是那些事。

### 该下哪个文件

每次发布都为各平台附一个压缩包。下面的链接始终指向最新版本,不会过期。

| 平台 | 压缩包 |
|---|---|
| Windows(Intel/AMD) | `tmon_windows_amd64.zip` |
| Windows(ARM) | `tmon_windows_arm64.zip` |
| macOS(Apple 芯片) | `tmon_darwin_arm64.tar.gz` |
| macOS(Intel) | `tmon_darwin_amd64.tar.gz` |
| Linux(x86-64) | `tmon_linux_amd64.tar.gz` |
| Linux(ARM64) | `tmon_linux_arm64.tar.gz` |

不确定架构就跑 `uname -m`:`x86_64` 是 amd64,`aarch64` 或 `arm64` 是 arm64。

### Windows

```powershell
$dest = "$HOME\bin"
New-Item -ItemType Directory -Force $dest | Out-Null
$url = 'https://github.com/Mengzhex/tmon/releases/latest/download/tmon_windows_amd64.zip'
Invoke-WebRequest -Uri $url -OutFile "$env:TEMP\tmon.zip"
Expand-Archive -Force "$env:TEMP\tmon.zip" -DestinationPath $dest
```

如果 `~\bin` 还不在 `PATH` 上就加进去,然后**开一个新终端**让改动生效:

```powershell
$user = [Environment]::GetEnvironmentVariable('PATH', 'User')
if ($user -notlike "*$dest*") {
    [Environment]::SetEnvironmentVariable('PATH', "$user;$dest", 'User')
}
```

### Linux

```sh
curl -fsSL https://github.com/Mengzhex/tmon/releases/latest/download/tmon_linux_amd64.tar.gz | tar xz
install -Dm755 tmon ~/.local/bin/tmon
```

大多数发行版已经把 `~/.local/bin` 放在 `PATH` 上了。如果 `command -v tmon` 是空的,加一行:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
```

想让整台机器都能用,改成 `sudo install -Dm755 tmon /usr/local/bin/tmon`。

### macOS

```sh
curl -fsSL https://github.com/Mengzhex/tmon/releases/latest/download/tmon_darwin_arm64.tar.gz | tar xz
mkdir -p /usr/local/bin && install -m755 tmon /usr/local/bin/tmon
```

**第一次运行会被 Gatekeeper 拦下**,因为这些二进制没有用 Apple 开发者证书签名。macOS 会报成*"tmon 已损坏,无法打开"*——这个提示是误导性的:文件没坏,只是带了隔离标记。清掉它:

```sh
xattr -d com.apple.quarantine /usr/local/bin/tmon
```

签名并公证可以免掉这一步,但需要付费的 Apple 开发者账号。现在没有,所以这里直接说清楚,而不是留给你自己去撞。

### 校验下载

每次发布都带一个 `checksums.txt`。Linux 上:

```sh
curl -fsSLO https://github.com/Mengzhex/tmon/releases/latest/download/checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

macOS 上用 `shasum -a 256 --ignore-missing -c checksums.txt`。Windows 上自己比对哈希:

```powershell
(Get-FileHash "$env:TEMP\tmon.zip" -Algorithm SHA256).Hash.ToLower()
```

### 从源码构建

需要 Go 1.25 或更新:

```sh
git clone https://github.com/Mengzhex/tmon.git
cd tmon
go build -o tmon ./cmd/tmon
```

交叉编译和发布目标见 [docs/BUILD.md](docs/BUILD.md)(英文)。

### 运行前提

| | |
|---|---|
| Windows | 10 1809 / Server 2019 及以上——更早的版本没有 ConPTY,而 tmon 正是通过它采集的 |
| macOS | 11 及以上 |
| Linux | 任意发行版;发布的二进制是静态的(CGO 关闭),所以 Alpine 这类 musl 系统也能跑 |
| shell | 任何 shell 都会被完整录下来。bash、zsh、PowerShell 额外能拿到按命令切分的结构——`last=`、工作目录、以及是哪条命令失败的 |

**平台状态。** 开发和测试套件跑在 Windows 上,伪终端那条路径被持续验证。POSIX 的 pty 路径每次发布都会编译并交叉校验,也被与平台无关的那部分测试覆盖,但**真实使用少得多**。它在 Linux 或 macOS 上出问题的话,值得反馈。

## 快速开始

两条命令。打开一个终端:

```sh
tmon start                 # 开
tmon start --port 7337     # ……并顺便在一个 URL 上提供 MCP,如果你的客户端需要
tmon end                   # 关
```

`tmon start` 一次把必须发生的三件事都做了,免得你逐个去发现:

1. 让**之后打开的每个终端**都自己录制(改一处 shell 启动文件,改前先备份,用 `tmon hook uninstall` 撤销)
2. 打印粘贴给 AI 客户端的配置——**一次**,不是每个终端一次
3. 录制**当前这个**终端,否则你敲命令的这扇窗口恰好会是唯一的盲区

然后照常干活,问你的 AI:*"刚才那个为什么失败了?"*

`tmon end` 是它精确的反操作:停止覆盖新终端,并停掉正在跑的录制。**已经录下来的一切仍然可读**——它关掉的是监控,不是历史。

关于 `end` 有一点要知道:录制和 shell 是同一个东西,因为 tmon 正是靠把 shell 包在 pty 里来录制的。所以停止一个录制**必然**结束它正在录的那个 shell,不存在"shell 活着但录制停了"这种安排。它是请每个录制器干净收尾而不是直接杀掉,所以最后那段输出是被刷盘而不是丢掉。只想停掉未来的终端、放过正在跑的,用 `--keep-shells`。

打印出来的配置提供三种接入方式,用你的平台支持的那种:

- **stdio** —— 客户端自己去启动 `tmon mcp`。不占端口、不用 token、不留任何常驻进程。这是默认,对大多数人来说够了。
- **Streamable HTTP**(`/mcp`)—— 当前的 URL 式传输。
- **SSE**(`/sse`)—— 更老的 HTTP+SSE 传输,但它仍然是很多托管平台**唯一**提供的那种。平台向你索要 "MCP SSE URL" 时,要的就是这个。

`tmon start --port 7337` 已经把这个端点跑起来了。如果要跑一个**不录制任何东西**的端点——比如某台机器只负责把别的终端录下的内容提供出去——直接用 `serve`:

```sh
tmon serve --port 7337   # 自己转后台,打印 URL,然后返回
```

它自己转后台,是因为端点打印完 URL 之后就没什么话要说了,不该占着你的终端。想让它留在前台、实时看日志,加 `--foreground`,用 Ctrl+C 结束。

```sh
tmon serve --status    # 有在跑吗,跑在哪
tmon serve --stop      # 从任意终端停掉它
```

转到后台的端点在启动它的那个终端关闭后依然活着,要说的话都写进 `~/.tmon/serve.log`。

**永远不要按进程名去停 tmon。** `taskkill /F /IM tmon.exe` 或 `pkill tmon` 会连带杀掉每个被录制终端背后的录制器,而录制器持有那个 shell 的 pty,**那些 shell 会跟着一起死——不相干的窗口会坏掉**。用 `tmon serve --stop` 或 `tmon end`,它们是请对方干净收尾。

所有 HTTP 形式默认只绑回环地址、要求 bearer token、并拒绝带浏览器 `Origin` 的请求。

### 助手在另一台机器上

```sh
tmon serve --bind 0.0.0.0 --port 7337 --allow 192.168.1.0/24
```

它会为每个网络接口、每种传输各打印一个 URL,SSE 那个里已经带上 token。用之前有两件事值得知道:

- **流量是明文 HTTP。** 录下的终端输出和 token 对任何能观测这段网络的人都是可读的,而脱敏只能抓它认得的凭据形态,不是全部。**把那个端口的可达范围当成你 scrollback 的可达范围来看待。**
- `--bind` 必须手敲。只写在配置文件里不会让端点对外发布,所以一个手滑留下的设置不可能意外暴露某个终端。

助手那台机器能开 SSH 隧道的话优先用隧道——不暴露端口,而且加密:

```sh
ssh -N -L 7337:127.0.0.1:7337 user@this-machine   # 在助手那台机器上执行
```

然后让助手连 `http://127.0.0.1:7337/sse`。

如果你压根不想要启动钩子,`tmon shell` 只录当前终端,别的什么都不改。

## 多个终端,一个端点

**没有任何按终端要配的东西。** 录制器是各自独立的进程,只往 `~/.tmon/sessions/` 写;一个端点读这个目录,所以你录的每个终端都从它那里可见。开十个终端,你依然只有一份 MCP 配置,而且没有任何东西需要清理。

让这件事真正可用的是**会话带身份**,几个终端因此彼此可辨——就是本页开头那个列表。

工作目录是最可靠的区分依据,而工具直接接受它:问 `cwd:webapp` 就行,不用去翻会话 id。当一个子串匹配到多个会话、而且其中不止一个还在录制时,tmon 会**报错并列出候选,而不是猜**。

身份从哪来:

| | 来源 | 可用性 |
|---|---|---|
| 标题 | `OSC 0`/`OSC 2` | 免费;伪终端自己就会发,不需要 shell 配合 |
| 工作目录 | tmon 自己的标记,或 `OSC 7` | 需要 bash、zsh 或 PowerShell;Windows 从不发 `OSC 7` |
| 上一条命令及其退出码 | 命令索引 | 需要 bash、zsh 或 PowerShell |
| **此刻**正在跑什么 | 一个尚未闭合的命令块 | 只有 bash 和 zsh——PowerShell 只在命令结束之后才上报它 |

默认情况下,列表显示所有仍在录制的终端,加上最近两小时内结束的,并**明确说明省略了多少个更旧的**。已结束的会话在 `buffer.retain_days`(默认 7)之后被删除,这个清理**从不触碰仍在录制的终端**。

## start 和 serve 不是二选一

`tmon start --port N` 覆盖了通常的情形,所以大多数人根本不需要敲 `tmon serve`。这两个命令都存在,是因为它们站在数据的两侧:

| | `tmon start` | `tmon serve` |
|---|---|---|
| 做什么 | **录制**一个终端 | 让 AI 在一个 URL 上**读**已录的内容 |
| 方向 | 产出数据 | 不产出任何数据 |
| 在哪跑 | 每个你想采集的终端里 | 一次,任意位置 |
| 必须吗 | 必须 | 只在你没用 `start --port` 时 |

没有 `start` 就没有任何可读的东西,这时 AI 问你的终端会**正确地**报告它什么都看不到。没有 `serve` 录制照旧进行,只是 URL 式客户端连不上。如果你的客户端接受了粘贴过去的 `command` 配置,它会自己启动 `tmon mcp`,`serve` 就完全不需要。

## 命令

| | |
|---|---|
| `tmon start [--label NAME] [--port N]` | 开:钩子 + 配置 + 录制当前终端 |
| `tmon end [--keep-hook] [--keep-shells]` | 关;已录的历史保留 |
| `tmon shell [--label NAME] [--quiet]` | 只录当前终端,别的什么都不改 |
| `tmon run -- ./deploy.sh` | 只录一条命令 |
| `tmon hook install \| uninstall \| status` | 让新开的终端自动录制 |
| `tmon sessions [--all] [--live] [--hours N]` | 录了什么,带 cwd 和各自跑过的命令 |
| `tmon tail [SESSION] [--lines N] [--raw]` | 自己看,不经过 AI |
| `tmon last-error [SESSION]` | 最近一条失败的命令及其输出 |
| `tmon mcp` | 在 stdin/stdout 上说 MCP(客户端自己跑这个) |
| `tmon serve [--foreground]` | 在 HTTP 端口上提供 MCP,不录制任何东西 |
| `tmon serve --status \| --stop` | 看端点在不在,或停掉它 |
| `tmon mcp-config [--format …]` | 再打印一次客户端配置 |
| `tmon token rotate` | 换掉 HTTP 的 bearer token |
| `tmon host add \| list \| verify \| query` | 目标主机的只读访问 |
| `tmon config path \| show \| init` | 配置 |
| `tmon version` | 这是哪个构建 |

`--home DIR` 对每条命令都有效,用来整体移动状态目录(默认 `~/.tmon` 或 `$TMON_HOME`)。`tmon help --all` 打印全部选项。

## 会话

给会话起个名字,之后就能按名字问它:

```sh
tmon shell --label deploy
```

助手用这些方式选会话:`latest`(默认,优先选还开着的)、`cwd:webapp`、`label:deploy`、`host:web-prod-1`,或者一个会话 id。开着好几个终端时通常该用 `cwd:`,因为工作目录才是真正区分它们的东西。会话归属到哪台主机,是靠配置文件里的 `sessions:` 规则决定的,不在命令行上。

当一个选择器匹配到多个会话、而且其中不止一个还活着时,tmon 会报错并列出它们,**而不是猜**——从错误的终端里答话比反问你要哪一个更糟。

当 shell 是 bash、zsh 或 PowerShell 时,tmon 还会记录每条命令从哪开始、到哪结束、退出码是多少。这正是把*"那个为什么失败了"*从"这是最后 500 行"变成**一次查找**的原因——最近一个非零退出,和它自己的输出。其他 shell 依然被完整采集,只是缺这份精度,而且 tmon 会在回答里说明这一点,不装作有。

## 使用技巧

**在长任务开始**之前**开,而不是之后。** 录制从 shell 启动的那一刻开始,所以一个已经在没录制的窗口里失败掉的构建,是找不回来的。`tmon start` 只需装一次钩子,之后你打开的每个终端都被覆盖——这是**不需要你记得**的那个版本。

**按目录问。** 开着好几个终端时,"webapp 里那个构建为什么失败"就能用,因为 `cwd:webapp` 会选中它。到处复制会话 id 是慢路;而当两个还活着的会话都匹配时,tmon 会拒绝猜,而不是从错误的终端里答话。

**`last-error` 比 `tail` 好用。** 在 bash、zsh、PowerShell 下 tmon 知道每条命令的起止,所以"最后一条失败的命令和它自己的输出"是**一次查找**。改成去取最后 500 行,等于把解析工作丢给助手,而它有时会解析错。

**给你还会回来看的东西起名字。** `tmon start --label deploy`,下周就能问 `label:deploy`——那时候工作目录早就不好记了。

**ssh 会话是录在这一侧的。** 服务器上不需要有 tmon。在被录制的 shell 里跑 `ssh`,远端输出就被本地捕获了。

**缓冲区按实测调,别猜。** `tmon sessions` 会打印每个会话真实的写入速率,以及它的缓冲现在能回溯多远。如果这个时长比"出问题"到"你来问"之间的间隔还短,就调大 `buffer.max_bytes`。详见 [docs/buffer-sizing.md](docs/buffer-sizing.md)。

**用它自己的命令去停它。** `tmon end`,或者只停端点用 `tmon serve --stop`。`taskkill /IM tmon.exe` 和 `pkill tmon` 会杀掉每个被录制终端背后的录制器,而录制器持有那个 shell 的 pty,**不相干的窗口会跟着一起死**。

**知道脱敏的边界在哪。** 它抓的是它**认识的**凭据形态,写入和读取两个方向各做一遍。**这不是保证。** 真正的密码提示会关掉回显,那些字符压根不会到达 pty;脱敏真正针对的,是你敲在命令行上、或者某个脚本自己打印出来的那个 token。

## 远程主机

```sh
tmon host add web-prod-1 --address 10.0.0.11 --user deploy --tier user
# 把它打印出来的探针和脚本拷过去,在目标机上执行那个脚本
tmon host verify web-prod-1
```

两个层级,**都由 sshd 强制执行**:

- `--tier root` —— 一个专用账号,加上 `sshd_config` 的 `Match` 块,**并且**在密钥上挂 forced command。两层独立的约束,所以一处失误不致命。需要权限的只读命令(`nginx -T`、监听端口背后的进程名)可用,走一份**精确命令、不含通配符**的 sudoers 白名单。
- `--tier user` —— 在你自己的 `~/.ssh/authorized_keys` 里多加一把带 `command="..."` 和 `restrict` 的密钥。**不需要 root**,而强制执行仍然由 sshd 做。需要权限的命令会报告自己不可用,而不是失败。

`tmon host verify` 是真正要紧的那一步。它**不是**读一遍配置然后宣布成功:它会真的去尝试执行一条命令、读 `/etc/passwd`、创建文件、申请一个终端、转发一个端口——**只有每一项都被拒绝才算通过**。**主机在通过之前无法被查询**,而失败的主机会被自动设回 `disabled`。

tmon 从不往目标主机写任何东西。装探针需要的权限恰恰是那把只读密钥没有、也不该有的,所以 `tmon host add` 生成密钥和一个安装脚本,由**你**用自己的凭据去执行。

## 缓冲区大小

每个会话有一个由分段文件组成的环形缓冲。写满时丢掉**最旧的整个分段**,所以剩下的永远是**一段连续不断的流**——中间不会出现空洞。每次读取都会分别报告环形缓冲是否已经丢弃过更旧的输出(`truncated`),以及是否检测到**真正的**不连续(`gap_detected`),这样"什么都没丢"是一个**可核查的断言**,而不是一个假设。

默认每个会话 256 MiB raw 加 64 MiB cooked。密集的日志刷屏大约 2–5 MB/min,所以这个量能回溯大约 1–2 小时。`tmon sessions` 会显示每个会话**实测**的速率和它的缓冲实际能回溯多远,方便你按真实负载来定大小。详见 [docs/buffer-sizing.md](docs/buffer-sizing.md)(英文)。

## 敏感输出

**在你屏幕上出现过的东西都在缓冲里。** 脱敏跑两遍——一遍在写入磁盘之前,一遍在任何内容返回给 AI 之前——覆盖对看起来像密钥的变量名的赋值、`Authorization` 头、AWS / GitHub / Slack / JWT 的形态、私钥块、`mysql -p`、`curl -u`,以及 URL 里的凭据。tmon 写出的一切都是仅属主可读。

但要清楚它的边界:**这是一个能抓住它认识的形态的过滤器,不是一个保证。** 真正的密码提示会关掉回显,那些字符压根不会到达 pty——脱敏真正针对的,是你敲在命令行上、或者某个脚本自己打印出来的凭据。

## 需求对照

| # | 需求 | 位置 |
|---|---|---|
| 1 | 只在被问时拉取;不监控、不告警、不推送 | `internal/mcpsrv` —— 没有定时器、没有订阅;`capabilities` 只声明工具 |
| 2 | AI 只读,在系统层强制 | `internal/probe`(封闭动词列表、不经 shell、固定 argv)+ sshd forced command;由 `tmon host verify` 证明 |
| 3 | 场景 A:近期输出与错误分析 | `get_last_error`、`get_recent_output`、`search_output`、`get_command_history` |
| 4 | 场景 B:主机事实用于部署建议 | `get_host_env`、`get_deploy_context` |
| 5 | 无损采集,不是截图 | `internal/record` 的 pty 采集;`internal/store` 的连续性校验 |
| 6 | 缓冲区大小可配 | `buffer.max_bytes` 全局、按 label 覆盖、全局磁盘上限 |
| 7 | 可选择查询哪个会话 | label、host、工作目录、id;`store.Resolve` 拒绝歧义匹配 |
| 8 | 多主机 | `hosts:` 列表,各自有独立的密钥、层级和验证状态 |
| — | 适用大部分终端 | 在 pty 上采集,使终端模拟器与此无关 |

## 文档

下面三份是给要改代码的人看的,保持英文:

- [docs/security-model.md](docs/security-model.md) —— 这里的"只读"指什么,以及它不覆盖什么
- [docs/buffer-sizing.md](docs/buffer-sizing.md) —— 怎么定缓冲区大小
- [docs/BUILD.md](docs/BUILD.md) —— 构建,以及这台机器上的工具链问题
