# エージェントタスク（MCP・プラグイン連携）

自分のPCで使っている Claude Code・Codex・ChatGPT から、家のUbuntu上の**使い捨てVM**にいるコーディングエージェントへ、時間のかかる作業を頼めるようにする機能です。VMの中のエージェントは、PCで既にログインしている codex / claude / opencode / pi / gemini などの認証情報をそのまま使います。自前で持つ「クラウドエージェント」だと考えてください。

[トップへ戻る](../README.md) · [文書一覧](README.md) · [Security](../SECURITY.md)

```text
Claude Code / Codex / ChatGPT（あなたのPC）
  │  MCP: runnerloom_start_task「このrepoでこれをやって」→ すぐタスクIDが返る
  ▼
runnerloom mcp serve（PC上）── 許可済みエージェントの認証ファイル・環境変数だけを読む
  │  mTLS（クライアント証明書 + Cluster CAのピン留め）
  ▼
Controller ── タスクを暗号化保存し、GitHub Jobと同じ資源台帳でNodeへ配置
  │  既存の /v1/sync（Node証明書）
  ▼
Node → 新しいVM（最大 executionMinutes）
         1. repoをclone、作業ブランチを作る（既にあれば続きから）
         2. エージェントCLIを導入して非対話で実行（runnerユーザー）
         3. commit → push → 必要ならdraft PR
         4. 結果をシリアルへ出して電源OFF → ホストが停止と削除を確認
  ▼
結果（branch・commit・PR URL・差分統計・最後の出力、pushできなければpatch）
  → runnerloom_get_task で後から受け取る。PCを閉じていても作業は続く
```

## 検証済みの範囲

| 部分 | 状態 |
|---|---|
| 投入・配置・暗号化保存・削除時の消去（SQLite） | 単体テスト |
| クライアント → Controller → Agent → 結果（実TLS・実HTTP・実SQLite、VMは偽物） | `internal/control` 統合テスト |
| ゲスト内ランナー（clone → エージェント → commit → push → 結果出力） | ローカルのbare repoに対して実スクリプトを実行するテスト。VMなし・ユーザー切替なし・ネットワークなし |
| MCP（stdio と Streamable HTTP） | プロトコル単位のテスト |
| 実VMでのタスク実行、各エージェントCLIの実際の導入・ログイン・実行 | **未検証** |
| Claude Code プラグイン、Codex、ChatGPT からの実接続 | **未検証**（設定例は各公式文書に基づく） |

組み込みのエージェント定義（下表）は、各CLIの公開文書にあるヘッドレス実行方法と認証ファイルの場所に基づきます。CLI側の変更に合わせて `agents.json` で上書きできます。

## 仕組み

### タスクとVMの関係

1タスク = 1VM です。GitHub Jobと同じ Instance として台帳に載り、同じ規則で扱われます。

- 配置は `tasks: true` の Pool だけ。GitHub の Scale Set には結び付きません。逆に GitHub の Pool をタスクに使うことも、タスク用 Pool に GitHub Runner を割り当てることもできません。
- CPU・RAM は**ホストが停止を確認するまで**、ディスクは**削除を確認するまで**保持します。取り消しやクライアント失効でも先に解放しません。
- VMの作成・起動意図の記録・所有確認・削除は、GitHub Runner VM と同じコードです。違うのは cloud-init に入れるジョブだけです。
- 空きがなければ `Queued` で待ちます。1時間配置できなければ `Expired` になり、預かった内容を消去します。

### タスクの状態

| 状態 | 意味 |
|---|---|
| Queued | 配置できるNodeを待っている |
| Starting | VMを準備・起動中 |
| Running | エージェント実行中。`progress` に最近の出力が入る |
| Stopping | 停止指示中（取り消し・時間切れ） |
| Collecting | VM停止を確認し、結果の到着待ち |
| Succeeded / Failed | 結果あり。エージェントの終了コードで分かれる |
| Cancelled / Expired / Finished | 取り消し / 待ち時間切れ / 結果なしで終了 |

### 結果の受け取り方

- **Repositoryあり**：`branch` へ push します。基点は `baseRef`、未指定なら既定ブランチです。`pullRequest` を指定すると、draft PR も作ります。同じ `branch`、または `continueFrom` で指定したタスクのブランチが既にあれば、その続きから作業します。VMは使い捨てのままで、状態はブランチに残ります。
- **pushできない場合**：GitHubトークンを許可していない、またはpushが競合した場合です。差分を patch（最大384KiB）として返します。
- **Repositoryなし**：空のディレクトリで作業し、作ったファイルを patch で返します。
- **共通**：エージェントの最後の出力（最大64KiB）と `git diff --stat` も返します。

## 信頼境界と認証情報

> [!WARNING]
> タスクを投げると、そのエージェントの**ログイン情報がVMに渡ります**。VMの中のエージェントは外部へ通信でき、sudoも使えます。VMに渡した認証情報は、VMの中のコードから読める前提で考えてください。

| 項目 | 扱い |
|---|---|
| PCから出るもの | `client allow` で許可したエージェントの、定義に書かれたファイルと環境変数だけ。`github` を許可した場合だけ GitHub トークン |
| 許可の変更 | PCの利用者がCLIで行う。MCP（モデル側）からは変更できない。プロンプトインジェクションで別のエージェントの認証情報を持ち出させないため |
| 通信 | Controllerの Cluster CA をピン留めした mTLS。クライアント証明書のURI種別はNodeと別で、クライアントはVM同期を、NodeはタスクAPIを使えない |
| Controllerでの保存 | 指示・認証ファイル・環境変数・トークンは master key の AES-GCM で暗号化。VM削除の確認後、待ち時間切れ、または配置前の取り消しで消去 |
| Nodeでの保存 | JITと同じく owner-only の seed と qemu専用の seed.iso。VM削除で消去 |
| VMの中 | 認証ファイルは runner のホーム配下に0600で置く。GitHubトークンはclone・push・PR作成にだけ使い、エージェントの環境変数には入れない（エージェントに渡したい場合は、プロファイルの `env` へ明示的に追加する） |
| 結果 | VMの出力は信用しない。上限を掛けて保存し、VMの状態遷移や削除の証拠には使わない |

**OAuthログインのトークン更新について**：VMのエージェントがトークンを更新すると、PC側のログインが切れることがあります。この機能はそれを許容する前提です。避けたい場合は、長期トークンや APIキーを環境変数で渡してください（例：`claude setup-token` の `CLAUDE_CODE_OAUTH_TOKEN`、`OPENAI_API_KEY`）。

**GitHubトークン**：対象repoだけに絞った fine-grained PAT（Contents と Pull requests を Read and write）を推奨します。VMは作業ブランチへpushしますが、トークンの権限内なら他のブランチにも書き込めてしまいます。`main` はブランチ保護で守ってください。

## セットアップ

### 1. タスク用Poolを追加する（Controller）

Cluster設定の `pools` に `tasks: true` の Pool を足し、使わせる Node の `allowedPools` にも加えます。`runnerName` は不要で、`warmIdle` は0です。`executionMinutes` がタスクの最長時間です。

```json
{
  "name": "agent-tasks",
  "image": "ubuntu-24",
  "vcpu": 4,
  "memoryMiB": 8192,
  "overheadMiB": 512,
  "rootGiB": 40,
  "scratchGiB": 0,
  "diskOverheadGiB": 2,
  "maxRunners": 2,
  "warmIdle": 0,
  "executionMinutes": 240,
  "enabled": true,
  "tasks": true
}
```

```bash
sudo runnerloom config plan --state "$RL_CONTROLLER_STATE" --file cluster.json
sudo runnerloom config apply --state "$RL_CONTROLLER_STATE" --plan <ID>
sudo systemctl restart runnerloom-controller.service
```

> [!IMPORTANT]
> タスク用 Pool を `allowedPools` に入れる Node は、先にこの版へ更新してください。古い Agent はタスクを含む指示を読めず、その Node の同期全体が止まります。

CPU・RAM・ディスクは GitHub Job と同じ予算から引かれます。Golden Image は GitHub 用と同じものを使えます。エージェントCLIと Node.js 22 は、タスク開始時にVMの中で導入します。Node.js は nodejs.org の SHASUMS256.txt で SHA-256 を照合します。導入を省きたい場合は、CLIを入れた Image を別に用意してください。

`controller run --offline` でもタスクは動きます。GitHub Actions を使わず、タスクだけに使う構成も可能です。

### 2. PCからControllerへ届くようにする

Claude Code を使うPCと Controller が同じ1台なら、既定の `https://127.0.0.1:8443` のままで構いません。別のPCから使う場合は、[MULTI_NODE.ja.md](MULTI_NODE.ja.md) と同じように、Controller を LAN から到達できるアドレスで待ち受けさせます。クライアントの通信は Node と同じ mTLS ポートを使います。

### 3. PCを登録する

秘密鍵はPCで作り、PCから出しません。Controller へ渡す CSR と、返ってくる bundle はどちらも公開情報です。

```bash
# PC
runnerloom client init --name laptop
#   → ~/.config/runnerloom/client/client.csr と csrHash が表示される

# Controller（CSRファイルを持ってきて）
sudo runnerloom client approve --state "$RL_CONTROLLER_STATE" \
  --csr /path/to/client.csr --name laptop \
  --url https://controller.example.lan:8443 --out /root/laptop-bundle.json
#   → 表示された csrHash がPCの値と同じことを確認する

# PC（bundleを持ってきて）
runnerloom client install --bundle /path/to/laptop-bundle.json
```

設定ディレクトリは `--client-dir` か `RUNNERLOOM_CLIENT_DIR` で変えられます。既定は `~/.config/runnerloom/client` です。失効は Controller で `runnerloom client revoke laptop` を実行します。

### 4. 送信してよいものを許可する（PC）

```bash
runnerloom client allow codex claude   # この2つの認証情報だけVMへ送ってよい
runnerloom client allow github         # push・PR用のGitHubトークンも送ってよい
runnerloom client defaults --pool agent-tasks --agent codex
```

GitHubトークンは `GH_TOKEN` / `GITHUB_TOKEN` から読みます。無ければ `~/.config/runnerloom/client/github-token`（0600）を使います。MCPクライアントが環境変数を渡さない場合に備えて、ファイルに置く方が確実です。

### 5. 手で試す

```bash
runnerloom task agents          # 各エージェントの許可と、見つかった認証情報（名前だけ）
runnerloom task pools
runnerloom task start --agent codex --repo example-org/private-app --pr \
  --prompt "テストが落ちている原因を調べて直し、テストを追加して"
runnerloom task show <ID> --wait 50s
runnerloom task start --continue <ID> --prompt "レビュー指摘に対応して"
```

## つなぎ方

MCPサーバーは `runnerloom mcp serve` です。PCで動き、上の client 設定を使います。

| ツール | 内容 |
|---|---|
| `runnerloom_list_agents` / `runnerloom_list_pools` | 使えるエージェント・許可・認証情報の有無 / VMサイズと時間上限 |
| `runnerloom_start_task` | 投げてすぐ戻る。`repository`・`baseRef`・`branch`・`pullRequest`・`continueFrom` |
| `runnerloom_get_task` | 状態・途中経過・結果。`waitSeconds` は最大50（ChatGPTの約60秒の上限に収めるため） |
| `runnerloom_list_tasks` / `runnerloom_cancel_task` | 一覧 / 取り消し |

### Claude Code（プラグイン）

このリポジトリはプラグインのマーケットプレイスも兼ねています。事前に `runnerloom` コマンドを PATH に入れ、手順3〜4を済ませてから導入してください。

```text
/plugin marketplace add MOVEI144/RunnerLoom
/plugin install runnerloom@runnerloom
```

MCPサーバーのほかに、`/runnerloom:vm-task` コマンドと、長い作業をVMへ任せる判断に使うスキルが入ります。

### Claude Code（手動）

```bash
claude mcp add --scope user runnerloom -- runnerloom mcp serve
```

### Codex CLI・ChatGPTデスクトップ

`~/.codex/config.toml` は Codex CLI と ChatGPT デスクトップアプリで共有されます。

```toml
[mcp_servers.runnerloom]
command = "runnerloom"
args = ["mcp", "serve"]
tool_timeout_sec = 90
# 環境変数の認証（GH_TOKEN、CLAUDE_CODE_OAUTH_TOKEN など）を使う場合だけ列挙
env_vars = ["GH_TOKEN"]
```

### ChatGPT（Web）

ChatGPT の Web 版はローカルの stdio サーバーを直接使えません。次のどちらかで接続します。いずれも ChatGPT の開発者モード（カスタムコネクタ）が必要です。

1. **OpenAI Secure MCP Tunnel（推奨）**：OpenAI の `tunnel-client` の stdio 転送に `runnerloom mcp serve` を指定します。外向き通信だけで、PCのポートを公開しません。手順は [OpenAIの文書](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels) に従ってください。
2. **HTTPS トンネル**：PCで HTTP 版を loopback だけで起動し、ngrok などで公開します。

   ```bash
   runnerloom mcp serve --http 127.0.0.1:8765
   #   秘密は ~/.config/runnerloom/client/mcp-secret（初回に0600で生成）
   ngrok http 8765
   ```

   コネクタの URL には `https://<トンネルのホスト>/mcp/<mcp-secret の中身>` を指定し、認証は「なし」を選びます。**このURLそのものが鍵です。** 共有しないでください。漏れた場合は `mcp-secret` を消して再起動すれば、新しい秘密に変わります。`Authorization: Bearer <secret>` ヘッダーも受け付けます（Codex の `bearer_token_env_var` など）。

`--http` は loopback 以外では待ち受けません。ChatGPT は読み取り専用のツールをそのまま実行します。`runnerloom_start_task` と `runnerloom_cancel_task` は書き込み扱いなので、実行前に確認を求めます。

## エージェント定義

| 名前 | 実行 | 送る認証（あるものだけ） |
|---|---|---|
| `claude` | `claude -p … --dangerously-skip-permissions` | `~/.claude/.credentials.json`、`CLAUDE_CODE_OAUTH_TOKEN`、`ANTHROPIC_API_KEY` |
| `codex` | `codex exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox …` | `~/.codex/auth.json`、`OPENAI_API_KEY`、`CODEX_API_KEY` |
| `opencode` | `opencode run …` | `~/.local/share/opencode/auth.json`、主要プロバイダのAPIキー |
| `pi` | `pi -p …` | `~/.pi/agent/auth.json`・`settings.json`・`models.json`、主要プロバイダのAPIキー |
| `gemini` | `gemini --yolo -p …` | `~/.gemini/oauth_creds.json` など、`GEMINI_API_KEY` |

VM自体が隔離の単位です。そのため、各CLIの承認プロンプトやサンドボックスは無効化して実行します。macOS の Claude Code は認証をキーチェーンに保存するので、`.credentials.json` がありません。`claude setup-token` で得た `CLAUDE_CODE_OAUTH_TOKEN` を使ってください。

### 自分で追加する（grok、musecode、agy など）

Grok Build・musecode・agy などは組み込んでいません。ヘッドレス実行の方法と認証ファイルの場所を確認し、`~/.config/runnerloom/client/agents.json` に定義してください（他ユーザーから書き込めない権限にします）。同じ名前を書けば組み込み定義も上書きできます。

```json
{
  "agents": [
    {
      "name": "my-agent",
      "description": "例: npmで配布されるCLI",
      "binary": "my-agent",
      "runtime": "node",
      "install": "npm install -g <パッケージ名>",
      "run": ["my-agent", "--non-interactive", "--prompt", "{prompt}"],
      "files": [{ "path": ".my-agent/credentials.json", "optional": true }],
      "env": ["MY_AGENT_API_KEY"]
    }
  ]
}
```

| 項目 | 意味 |
|---|---|
| `run` | VMの作業ディレクトリで実行する argv。`{prompt}` は指示の本文、`{promptFile}` は指示を書いたファイルのパスに置き換わる |
| `install` | `binary` が見つからないときだけ root で実行するシェル |
| `runtime` | `node` なら Node.js 22 以上を用意してから `install` する |
| `files` | `path` はホーム配下の相対パス（VM側も同じ場所）。PC側の場所が違うときは `from`（絶対パスか `~/`）で指定する。`work/` と `.runnerloom/` 配下は使えない |
| `env` | PCで値が入っていれば送る環境変数名 |

## 制限

- 1タスクの上限は、指示64KiB、認証ファイル合計256KiB（1ファイル128KiB）、暗号化前のタスク全体512KiB。
- 結果は、出力64KiB、patch384KiB、途中経過32KiB まで。越えた分は切り詰め、`truncated` を立てる。
- 1クライアントの未完了タスクは64件まで。配置待ちは1時間まで。
- clone できる Repository は https のみ（URLに認証情報を含めない）。PR作成は github.com のみ。
- 途中経過は約30秒ごとの更新。エージェントとの対話（追加の質問への回答など）はできない。続きは `continueFrom` で新しいタスクとして投げる。
