# エージェントタスク（MCP・プラグイン連携）

自分のPCの Claude Code・Codex・ChatGPT から、家のUbuntu上の**使い捨てVM**にいるコーディングエージェントへ、時間のかかる作業を頼めます。VMの中のエージェントは、PCで既にログインしている codex / claude / opencode / pi / gemini / grok / musecode などの認証情報をそのまま使います。自前の「クラウドエージェント」だと考えてください。

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
         2. エージェントCLIを導入し、runnerユーザーで非対話実行（既定でsudoなし）
         3. runnerとしてcommit → runnerのプロセスを全て止める
         4. rootが別の綺麗なrepoへ取り込み、トークンでpush → 必要ならdraft PR
         5. 結果をシリアルへ出して電源OFF → ホストが停止と削除を確認
  ▼
結果（branch・commit・PR URL・差分統計・最後の出力、pushできなければpatch）
  → runnerloom_get_task で後から受け取る。PCを閉じていても作業は続く
```

## 検証済みの範囲

| 部分 | 状態 |
|---|---|
| 投入・配置・暗号化保存・削除時の消去・失効（SQLite） | 単体テスト |
| クライアント → Controller → Agent → 結果（実TLS・実HTTP・実SQLite、VMは偽物） | `internal/control` 統合テスト |
| ゲスト内ランナー（clone → エージェント → commit → push → 結果出力） | ローカルのgitサーバーに対して実スクリプトを実行するテスト |
| 実VMでのタスク実行経路 | CI の実Ubuntu VMで `smoke-vm --task` を毎回実行。runnerユーザーへの切替、sudoなし、トークンがエージェントから読めないこと、github.comの公開repoのclone、無効トークンでのpush失敗、patchの返却、VM削除までを確認 |
| MCP（stdio と Streamable HTTP） | プロトコル単位のテスト |
| 各エージェントCLIの実際の導入・ログイン・実行 | **未検証**。公開文書にある導入方法・ヘッドレス実行・認証ファイルの場所に基づく |
| Claude Code プラグイン、Codex、ChatGPT からの実接続 | **未検証**（設定例は各公式文書に基づく） |

組み込みのエージェント定義は、CLI側の変更に合わせて `agents.json` で上書きできます。

## 仕組み

### タスクとVMの関係

1タスク = 1VM です。GitHub Jobと同じ Instance として台帳に載り、同じ規則で扱われます。

- 配置先は `tasks: true` の Pool だけです。GitHub の Scale Set には結び付きません。GitHub の Pool をタスクに使うことも、タスク用 Pool に GitHub Runner を割り当てることもできません。
- CPU・RAM は**ホストが停止を確認するまで**、ディスクは**削除を確認するまで**保持します。取り消しやクライアント失効でも、先に解放はしません。
- VMの作成・起動意図の記録・所有確認・削除は、GitHub Runner VM と同じコードです。違うのは cloud-init に入れるジョブだけです。
- 空きがなければ `Queued` で待ちます。同じPoolでは古いタスクから順に配置します。1時間配置できなければ `Expired` になり、預かった内容を消去します。
- ホストは、残り時間が10分未満のタスクVMを起動しません。

### タスクの状態

| 状態 | 意味 |
|---|---|
| Queued | 配置できるNodeを待っている |
| Starting | VMを準備・起動中 |
| Running | エージェント実行中。`progress` に最近の出力が入る |
| Stopping | 停止指示中（取り消し・時間切れ・失効） |
| Collecting | VMの停止または削除を確認し、結果の到着を待っている。削除後も2分は待つ |
| Succeeded / Failed | 結果あり。エージェントの終了コードで分かれる |
| Cancelled / Expired | 取り消し / 配置待ちの時間切れ |
| Finished | VMは終わったが、結果が届かなかった（VM内の異常など） |

結果が既にあるタスクや、削除中・削除済みのVMのタスクは、取り消しても状態が変わりません。

### 結果の受け取り方

- **Repositoryあり**：`branch` へ push します。基点は `baseRef`、未指定なら既定ブランチです。`pullRequest` を指定すると draft PR も作ります。同じ `branch`、または `continueFrom` で指定したタスクのブランチが既にあれば、その続きから作業します。VMは使い捨てのままで、状態はブランチに残ります。作業ブランチが既定ブランチや `baseRef` と同じ名前の場合は、作業せずに失敗します。
- **pushできない場合**：GitHubトークンを許可していない、github.com以外、またはpushが競合した場合です。差分を patch（最大384KiB）で返します。
- **Repositoryなし**：空のディレクトリで作業し、作ったファイルを patch で返します。
- **共通**：エージェントの最後の出力（約60KiB）と `git diff --stat` も返します。PR本文には指示を載せません（タスクIDとエージェント名だけ）。

結果はVMが出したもので、信用しません。Controllerは大きさを制限し、ブランチ名がタスクのものか、PR URLが対象repoの `https://github.com/<owner>/<repo>/pull/<番号>` かを確かめてから保存します。MCPの応答にも、出力は信用できないデータだと明記します。

## 信頼境界と認証情報

> [!WARNING]
> タスクを投げると、そのエージェントの**ログイン情報がVMに渡ります**。VMの中のエージェントは任意のコードを実行でき、**外部への通信も制限しません**。エージェント自身の認証情報は、VMの中のコード（repoのビルドスクリプトや、指示に紛れ込んだ悪意ある文章に従ったエージェント）から読める前提で考えてください。

| 項目 | 扱い |
|---|---|
| PCから出るもの | `client allow` で許可したエージェントの、定義に書かれたファイルと環境変数だけ。`github` を許可し、対象が github.com の場合だけ GitHub トークン |
| 読まないファイル | ホーム外、および `.ssh/`・`.gnupg/`・`.aws/`・`.kube/`・`.docker/`・`.config/gh/`・`.password-store/`・`.config/runnerloom/`・`.netrc`・`.git-credentials`・`.pgpass`・シェル履歴。定義に書かれていても送らない |
| 許可の変更 | PCの利用者がCLIで行う。MCP（モデル側）からは変更できない。プロンプトインジェクションで、別のエージェントの認証情報を持ち出させないため |
| 定義の固定 | `agents.json` の定義は許可した時点の内容で固定する。後から書き換わると `AGENT_CHANGED` で拒否し、`client allow` のやり直しを求める |
| 通信 | Controllerの Cluster CA をピン留めした mTLS。クライアント証明書のURI種別はNodeと別で、クライアントはVM同期を、NodeはタスクAPIを使えない |
| Controllerでの保存 | 指示・認証ファイル・環境変数・トークンは、master key の AES-GCM で暗号化。VM削除の確認後、待ち時間切れ、配置前の取り消し、またはクライアント失効で消去する。タスクのタイトル（指示の先頭）は一覧表示用に平文で残る |
| Nodeでの保存 | JITと同じく、owner-only の seed と qemu専用の seed.iso。VM削除で消去。シリアルログは結果の回収後に削除 |
| VMの中 | 認証ファイルは runner のホーム配下に0600で置く。エージェントは runner ユーザーで、**sudoなし**で動く。cloud-init のuser-dataとタスク本体は、エージェント起動前に消す |
| GitHubトークン | clone と、rootによる push・PR作成にだけ使う。エージェントの環境変数・ファイル・`.git/config` には入れない。エージェント終了後は runner の全プロセスを止め、rootが別の綺麗なrepoへコミットを取り込んでからpushする。repo内のhookや設定は、トークンを持つgitに読ませない |

**sudoを許す場合**：`agents.json` のプロファイルに `"sudo": true` を書くと、エージェントは root になれます。この場合、エージェントはトークンを含むVM内の全てを読めます。GitHubトークンを守りたいなら使わないでください。必要なパッケージは、事前に Image へ入れる方が安全です。

**OAuthログインのトークン更新**：VMのエージェントがトークンを更新すると、PC側のログインが切れることがあります。この機能はそれを許容する前提です。避けたい場合は、長期トークンや APIキーを環境変数で渡してください（例：`claude setup-token` の `CLAUDE_CODE_OAUTH_TOKEN`、`CODEX_API_KEY`）。

**GitHubトークン**：対象repoだけに絞った fine-grained PAT（Contents と Pull requests を Read and write）を推奨します。VMは作業ブランチへpushしますが、トークンの権限内なら他のブランチにも書き込めます。`main` はブランチ保護で守ってください。

**外向き通信**：VMからの通信は、GitHub Runner VMと同じく制限しません。エージェントが認証情報を外部へ送ることは技術的に防げません。信頼できるエージェントと指示だけを使ってください。

## セットアップ

### 1. タスク用Poolを追加する（Controller）

Cluster設定の `pools` に `tasks: true` の Pool を足し、使わせる Node の `allowedPools` にも加えます。`runnerName` は不要で、`warmIdle` は0です。`executionMinutes`（30以上）がタスクの最長時間です。

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

**タスクだけに使う構成**：GitHub Actions を使わないなら、Cluster設定の `github` を省けます。タスク用 Pool だけの設定では GitHub の管理は起動しません。`controller run --offline` でもタスクは動きます。

**Nodeで一度試す**：Node の準備ができたら、実VMでタスクの経路を確かめられます。固定のシェル「エージェント」で、ユーザー切替・秘密の隔離・github.com の公開repoのclone・push失敗・patch返却を確認し、VMを削除します。12分以上のタイムアウトが必要です。`$RL_NODE_CONFIG` は [OPERATIONS.ja.md](OPERATIONS.ja.md) と同じNode設定です。

```bash
sudo runnerloom smoke-vm --task --config "$RL_NODE_CONFIG" \
  --image /path/to/ubuntu-24.04.qcow2 --digest sha256:<Imageのsha256> --timeout 15m
```

### 2. PCからControllerへ届くようにする

Claude Code を使うPCと Controller が同じ1台なら、既定の `https://127.0.0.1:8443` のままで構いません。別のPCから使う場合は、[MULTI_NODE.ja.md](MULTI_NODE.ja.md) と同じように、Controller を LAN から到達できるアドレスで待ち受けさせます。クライアントの通信は、Node と同じ mTLS ポートを使います。

### 3. PCを登録する

秘密鍵はPCで作り、PCから出しません。受け渡すのは公開情報だけですが、途中での差し替えを防ぐため、**指紋を別の経路で**照合します。

```bash
# PC
runnerloom client init --name laptop
#   → ~/.config/runnerloom/client/client.csr と csrHash が表示される

# Controller（CSRファイルを持ってきて。csrHashはPCの画面から読み上げる等）
sudo runnerloom client approve --state "$RL_CONTROLLER_STATE" \
  --csr /path/to/client.csr --csr-hash <PCのcsrHash> --name laptop \
  --url https://controller.example.lan:8443 --out /root/laptop-bundle.json
#   → caFingerprint が表示される

# PC（bundleを持ってきて。caFingerprintはControllerの画面から）
runnerloom client install --bundle /path/to/laptop-bundle.json --ca-fingerprint <caFingerprint>
```

- `--url` は、Controller の証明書に入っている名前（`controller run` の `--advertise` や待ち受けアドレス）と同じにします。`client approve` はそのURLへ実際に接続して確かめます。Controller が止まっている場合だけ `--skip-url-check` で省けます。
- `--out` は新規ファイルだけです。同じ名前のPCを登録し直すときは `--replace` を付けます。失効した名前は再利用できません。
- 別の Controller に登録し直すときは、`client install` に `--replace` を付けます。
- クライアント証明書は、期限が近づくと自動で更新します。古い証明書も7日間は使えます。

設定ディレクトリは `--client-dir` か `RUNNERLOOM_CLIENT_DIR` で変えられます。既定は `~/.config/runnerloom/client` です。

PCを手放したときは、Controller で失効させます。そのPCの未完了タスクは取り消され、実行中のVMにも停止を指示します。

```bash
sudo runnerloom client revoke laptop --state "$RL_CONTROLLER_STATE"
```

### 4. 送信してよいものを許可する（PC）

```bash
runnerloom client allow codex claude   # この2つの認証情報だけVMへ送ってよい
runnerloom client allow github         # push・PR用のGitHubトークンも送ってよい
runnerloom client defaults --pool agent-tasks --agent codex
```

GitHubトークンは、次の順で最初に見つかったものを使います。

1. `~/.config/runnerloom/client/github-token`（0600）
2. `RUNNERLOOM_GITHUB_TOKEN`
3. `GH_TOKEN`
4. `GITHUB_TOKEN`

MCPクライアントは環境変数を渡さないことが多いので、ファイルに置く方が確実です。トークンを送るのは、対象が github.com のrepoの場合だけです。

### 5. 手で試す

```bash
runnerloom task agents          # 各エージェントの許可と、見つかった認証情報（名前だけ）
runnerloom task pools
runnerloom task start --agent codex --repo example-org/private-app --pr \
  --prompt "テストが落ちている原因を調べて直し、テストを追加して"
runnerloom task show <ID> --wait 50s
runnerloom task start --continue <ID> --prompt "レビュー指摘に対応して"
```

### macOS のPCから使う

リリースには macOS（Apple Silicon と Intel）向けのクライアント（`runnerloom-<版>-darwin-arm64.tar.gz` / `darwin-amd64.tar.gz`）があります。展開した `runnerloom` を PATH に置けば、`client`・`task`・`mcp serve` を使えます。macOS では Controller・Node は動かしません。署名・公証はしていないので、初回は Gatekeeper の許可が必要です。Windows のクライアントは、この版にはありません。

## つなぎ方

MCPサーバーは `runnerloom mcp serve` です。PCで動き、上の client 設定を使います。

| ツール | 内容 |
|---|---|
| `runnerloom_list_agents` / `runnerloom_list_pools` | 使えるエージェント・許可・認証情報の有無 / VMサイズと時間上限 |
| `runnerloom_start_task` | 投げてすぐ戻る。`repository`・`baseRef`・`branch`・`pullRequest`・`continueFrom` |
| `runnerloom_get_task` | 状態・途中経過・結果。`waitSeconds` は最大50（ChatGPTの約60秒の上限に収めるため） |
| `runnerloom_list_tasks` / `runnerloom_cancel_task` | 一覧 / 取り消し |

### Claude Code（プラグイン）

このリポジトリは、プラグインのマーケットプレイスも兼ねています。先に `runnerloom` コマンドを PATH に入れ、手順3〜4を済ませてから導入してください。

```text
/plugin marketplace add MOVEI144/RunnerLoom
/plugin install runnerloom@runnerloom
```

MCPサーバーのほかに、`/runnerloom:vm-task` コマンドと、長い作業をVMへ任せるか判断するスキルが入ります。

### Claude Code（手動）

```bash
claude mcp add --scope user runnerloom -- runnerloom mcp serve
```

### Codex CLI・ChatGPTデスクトップ

`~/.codex/config.toml` は、Codex CLI と ChatGPT デスクトップアプリで共有されます。

```toml
[mcp_servers.runnerloom]
command = "runnerloom"
args = ["mcp", "serve"]
tool_timeout_sec = 90
# 環境変数の認証（GH_TOKEN、CLAUDE_CODE_OAUTH_TOKEN など）を使う場合だけ列挙
env_vars = ["GH_TOKEN"]
```

### ChatGPT（Web）

ChatGPT の Web 版は、ローカルの stdio サーバーを直接使えません。次のどちらかで接続します。どちらも ChatGPT の開発者モード（カスタムコネクタ）が必要です。

1. **OpenAI Secure MCP Tunnel（推奨）**：OpenAI の `tunnel-client` の stdio 転送に `runnerloom mcp serve` を指定します。外向き通信だけで、PCのポートを公開しません。手順は [OpenAIの文書](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels) に従ってください。
2. **HTTPS トンネル**：PCで HTTP 版を loopback だけで起動し、ngrok などで公開します。

   ```bash
   runnerloom mcp serve --http 127.0.0.1:8765
   #   秘密は ~/.config/runnerloom/client/mcp-secret（初回に0600で生成）
   ngrok http 8765
   ```

   コネクタの URL には `https://<トンネルのホスト>/mcp/<mcp-secret の中身>` を指定し、認証は「なし」を選びます。**このURLそのものが鍵です。** 共有しないでください。漏れた場合は、`mcp-secret` を消して再起動すれば新しい秘密に変わります。`Authorization: Bearer <secret>` ヘッダーも受け付けます（Codex の `bearer_token_env_var` など）。

`--http` は loopback 以外では待ち受けません。ブラウザからの要求（`Origin` 付き）は、`--allow-origin` で許可したものだけ受け付けます。ChatGPT は読み取り専用のツールをそのまま実行します。`runnerloom_start_task` と `runnerloom_cancel_task` は書き込み扱いなので、実行前に確認を求めます。

## エージェント定義

| 名前 | 実行 | 送る認証（あるものだけ） |
|---|---|---|
| `claude` | `claude -p … --dangerously-skip-permissions` | `~/.claude/.credentials.json`、`CLAUDE_CODE_OAUTH_TOKEN`、`ANTHROPIC_API_KEY` |
| `codex` | `codex exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox …` | `~/.codex/auth.json`、`CODEX_API_KEY` |
| `opencode` | `opencode run --auto …` | `~/.local/share/opencode/auth.json`、`~/.config/opencode/opencode.json`、主要プロバイダのAPIキー |
| `pi` | `pi -p …` | `~/.pi/agent/auth.json`・`settings.json`・`models.json`、主要プロバイダのAPIキー |
| `gemini` | `gemini --yolo --skip-trust -p …` | `~/.gemini/oauth_creds.json`・`google_accounts.json`・`settings.json`、`GEMINI_API_KEY`、Vertex AI の環境変数 |
| `grok`（Grok Build） | `grok --always-approve --no-auto-update --prompt-file …` | `~/.grok/auth.json`・`config.toml`、`XAI_API_KEY` |
| `musecode` | `muse exec --yolo --prompt-file …` | `~/.config/muse/auth.json`・`settings.json`、`META_API_KEY` |
| `agy`（Antigravity） | `agy -p … --dangerously-skip-permissions` | `GEMINI_API_KEY` のみ。アカウントのログインはOSのキーリングにあり、コピーできない |

VM自体が隔離の単位です。そのため、各CLIの承認プロンプトやサンドボックスは無効にして実行します。macOS の Claude Code は認証をキーチェーンに保存するので、`.credentials.json` がありません。`claude setup-token` で得た `CLAUDE_CODE_OAUTH_TOKEN` を使ってください。`grok`・`musecode`・`agy` は、各社のインストーラーをVMの中で実行して導入します。

### 自分で追加する・上書きする

ヘッドレス実行の方法と認証ファイルの場所を確認し、`~/.config/runnerloom/client/agents.json` に定義してください（他ユーザーから書き込めない権限にします）。同じ名前を書けば、組み込み定義も上書きできます。定義を変えたら `runnerloom client allow <名前>` をやり直します。

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
| `install` | `binary` が見つからないときだけ、root で実行するシェル |
| `runtime` | `node` なら、Node.js 22 以上を用意してから `install` する |
| `files` | `path` はホーム配下の相対パス（VM側も同じ場所）。PC側の場所が違うときは、`from`（ホーム配下の絶対パスか `~/`）で指定する。`work/` と `.runnerloom/` 配下、上の「読まないファイル」は使えない |
| `env` | PCで値が入っていれば送る環境変数名 |
| `sudo` | `true` でエージェントに root を許す。トークンの隔離がなくなる（上記） |

## 制限

- 1タスクの上限は、指示64KiB、認証ファイル合計256KiB（1ファイル128KiB）、暗号化前のタスク全体512KiB。
- 結果は、出力約60KiB、patch384KiB、途中経過32KiB まで。越えた分は切り詰め、`truncated` を立てる。
- 1クライアントの未完了タスクは64件まで。配置待ちは1時間まで。
- clone できる Repository は https のみ（URLに認証情報を含めない）。GitHubトークンの送信と PR作成は github.com のみ。
- 途中経過は約30秒ごとの更新。エージェントとの対話（追加の質問への回答など）はできない。続きは `continueFrom` で新しいタスクとして投げる。
- VMの外向き通信は制限しない。
- クライアントは Linux と macOS のみ。
