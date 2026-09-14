# RunnerLoomドキュメント

このページは、RunnerLoomの文書を「ファイル名」ではなく「やりたい作業」から探すための案内です。

[トップへ戻る](../README.md)

## 初めて使う人の読む順番

1. [INSTALL.md](INSTALL.md) でCLIをインストールし、版とchecksumを確認する
2. [QUICKSTART.ja.md](QUICKSTART.ja.md) で1台構成を作り、最初のGitHub Jobを実行する
3. 途中で止まったら [TROUBLESHOOTING.ja.md](TROUBLESHOOTING.ja.md) で確認コマンドと原因を探す
4. 安定稼働後に [OPERATIONS.ja.md](OPERATIONS.ja.md) で日常運用、backup、更新手順を確認する
5. 2台目を追加するときだけ [MULTI_NODE.ja.md](MULTI_NODE.ja.md) へ進む

> [!TIP]
> 最初から全ファイルを読む必要はありません。1台構成なら、まず `INSTALL.md` と `QUICKSTART.ja.md` の2つで十分です。

## 作業から探す

| 作業 | 文書 | その文書で分かること |
|---|---|---|
| CLIを入れる | [INSTALL.md](INSTALL.md) | Release取得、checksum、`.deb` / archive、更新、削除 |
| 1台で始める | [QUICKSTART.ja.md](QUICKSTART.ja.md) | GitHub App、Golden Image、設定、network、service、初回Job |
| 対話メニュー | [INTERACTIVE_CLI.ja.md](INTERACTIVE_CLI.ja.md) | 引数なしTTY起動、確認語、dry-run-first、`--json` との非併用 |
| エラーを調べる | [TROUBLESHOOTING.ja.md](TROUBLESHOOTING.ja.md) | `doctor`、`github check`、`pool explain`、journal、代表的な理由コード |
| Nodeを増やす | [MULTI_NODE.ja.md](MULTI_NODE.ja.md) | LAN用Controller URL、招待、承認、mTLS、任意の自動発見 |
| 運用する | [OPERATIONS.ja.md](OPERATIONS.ja.md) | status、drain、Job停止、ログ、backup、maintenance、upgrade |
| Image cacheを管理する | [CACHE.ja.md](CACHE.ja.md) | 容量、完全性、dry-run prune、1台構成のhard-link重複排除 |
| 構造を理解する | [ARCHITECTURE.md](ARCHITECTURE.md) | Controller、Agent、state machine、資源会計、通信境界 |
| Image方針を確認する | [GOLDEN_IMAGE_POLICY.md](GOLDEN_IMAGE_POLICY.md) | Golden Imageに残す機能、maskするservice、qualification |
| セキュリティを評価する | [SECURITY.md](../SECURITY.md) | threat model、秘密情報、特権境界、報告方法 |
| 検証済み範囲を確認する | [VERIFICATION.md](VERIFICATION.md) | mockと実VMの区別、合格済みCI、未検証領域 |
| テストの意味を知る | [TESTING.md](TESTING.md) | Local commands, CI jobs, what a green check does not prove |
| v1の残作業を見る | [V1_ACCEPTANCE.md](V1_ACCEPTANCE.md) | 実環境で必要な受入試験と証拠 |
| 変更に参加する | [CONTRIBUTING.md](../CONTRIBUTING.md) | `make check`, self-review, required tests by change type, prohibitions |

## セットアップ文書の役割分担

### INSTALL.md

CLIをホストへ置くところまでを扱います。GitHub App、VM network、systemd serviceは作りません。

### QUICKSTART.ja.md

1台のUbuntu PCをControllerとNodeの兼用にし、手動Workflowが成功してVMが消えるところまでを扱います。

### MULTI_NODE.ja.md

動作済みのControllerへ2台目以降のNodeを参加させる手順だけを扱います。最初の1台には不要です。

### OPERATIONS.ja.md

構築後の操作を扱います。初回セットアップの途中ではなく、最初のJobが成功してから読む想定です。

### CACHE.ja.md

Controller/Node cacheの保存先、容量会計、再開download、安全な整理と1台構成の重複排除を扱います。

## 最低限覚える用語

| 用語 | 一文での意味 |
|---|---|
| Controller | GitHub需要、配置判断、状態DB、Node認証を管理する |
| Node / Agent | VMとホスト資源を実際に操作するUbuntuマシン |
| Pool | Workflowから選ぶ、管理者定義済みのVMサイズとRunner名 |
| Golden Image | UbuntuとActions Runnerを含む未登録のVM元イメージ |
| Cluster configuration | Node、Pool、Image、GitHub許可をまとめたJSON |
| Node configuration | 1台のNode固有の保存先、上限、Controller URLを持つJSON |

## 「完了」の意味

RunnerLoomでは、次の状態を区別します。

| 状態 | 意味 |
|---|---|
| CLI installed | `runnerloom` コマンドを実行できるだけ |
| Configured | `setup --apply` で設定とIDを保存した |
| Host ready | libvirt、KVM、専用network、imageが利用できる |
| GitHub ready | App権限、Runner Group、Repository許可をAPIで確認できた |
| Service running | ControllerとAgentのsystemd unitが起動している |
| Deployment accepted | 実Job、Artifact、停止、VM削除、隔離、復旧を対象ホストで確認した |

`setup --apply` の成功だけでは、Host readyやGitHub readyを意味しません。

## 情報の優先順位

仕様や文書が食い違う場合は、次の順で確認してください。

1. 対象Releaseの `VERSION`、release notes、`BUILDINFO.json`
2. 同じReleaseに含まれる `runnerloom --help --json` と `runnerloom config schema`
3. このディレクトリの手順書
4. 設計上の将来像やIssue

CLIのflag名は `runnerloom --help --json`、設定キーは `runnerloom config schema` を最終的な基準にしてください。
